package gitimport

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// runAskpass writes the real askpass script and runs it with a given prompt +
// expected host, returning what it would feed git.
func runAskpass(t *testing.T, prompt, expectHost string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "askpass.sh")
	if err := os.WriteFile(script, []byte(askpassScript), 0o700); err != nil {
		t.Fatal(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	pw.WriteString("THE_TOKEN")
	pw.Close()
	cmd := exec.Command("/bin/sh", script, prompt)
	cmd.Env = []string{"GIT_EXPECT_HOST=" + expectHost, "GIT_PUSH_USER=x-access-token"}
	cmd.ExtraFiles = []*os.File{pr} // fd 3, as authEnv passes it
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("askpass run: %v", err)
	}
	return string(out)
}

func TestAskpassHostCheck(t *testing.T) {
	cases := []struct {
		prompt, expectHost, want string
	}{
		// realistic GitHub prompts — exact host match emits the secret
		{"Username for 'https://github.com':", "github.com", "x-access-token"},
		{"Password for 'https://user@github.com':", "github.com", "THE_TOKEN"},
		{"Password for 'https://github.com':", "github.com", "THE_TOKEN"},
		{"Password for 'https://github.com:443':", "github.com", "THE_TOKEN"}, // port stripped
		// hostile / mismatched hosts emit NOTHING
		{"Password for 'https://github.com.evil.com':", "github.com", ""},
		{"Password for 'https://evilgithub.com':", "github.com", ""},
		{"Password for 'https://github.com@evil.com':", "github.com", ""}, // userinfo trick -> host is evil.com
		{"Password for 'https://evil.com':", "github.com", ""},
		// malformed / unknown prompts emit NOTHING
		{"Password: ", "github.com", ""},
		{"enter your password please", "github.com", ""},
		{"", "github.com", ""},
	}
	for _, c := range cases {
		got := runAskpass(t, c.prompt, c.expectHost)
		if got != c.want {
			t.Errorf("prompt %q (expect %s): got %q want %q", c.prompt, c.expectHost, got, c.want)
		}
	}
}

// initRepo makes a real repo shaped like a sandboxd clone (git init + origin + upstream).
func initRepo(t *testing.T) (repo string, mustGit func(args ...string)) {
	t.Helper()
	repo = t.TempDir()
	mustGit = func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+repo)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustGit("init", "-q")
	mustGit("remote", "add", "origin", "https://github.com/o/r")
	mustGit("config", "branch.main.remote", "origin")
	mustGit("config", "branch.main.merge", "refs/heads/main")
	mustGit("config", "user.name", "Dev")
	mustGit("config", "user.email", "dev@example.com")
	mustGit("config", "pull.rebase", "false")
	mustGit("config", "advice.detachedHead", "false")
	return repo, mustGit
}

// audit runs the host-side layout check + allow-list audit the way openAudited does.
func audit(t *testing.T, repo string) ([]string, error) {
	t.Helper()
	rc, err := openRepo(context.Background(), repo)
	if err != nil {
		return nil, err
	}
	return auditConfig(context.Background(), rc, repo)
}

func TestAuditConfigAllowsCloneConfig(t *testing.T) {
	repo, _ := initRepo(t)
	if f, err := audit(t, repo); err != nil || len(f) != 0 {
		t.Fatalf("clone-shaped repo flagged %v (err %v)", f, err)
	}
}

func TestAuditConfigRefusesKeysOutsideAllowList(t *testing.T) {
	cases := [][]string{
		{"http.extraHeader", "X: y"},
		{"http.proxy", "http://evil:8080"},
		{"remote.x.url", "https://evil.tld/r"},
		{"remote.https://github.com/o/r.url", "https://evil.tld/r"},
		{"remote.origin.pushurl", "https://evil.tld/r"},
		{"remote.origin.promisor", "true"},
		{"url.https://evil.tld/.insteadOf", "https://github.com/"},
		{"credential.helper", "!evil"},
		{"core.askPass", "/tmp/evil"},
		{"core.sshCommand", "ssh -i /tmp/x"},
		{"core.fsmonitor", "true"},
		{"core.hooksPath", "/tmp/hooks"},
		{"core.alternateRefsCommand", "evil"},
		{"filter.lfs.process", "evil"},
		{"submodule.x.url", "https://evil.tld/r"},
	}
	for _, c := range cases {
		repo, mustGit := initRepo(t)
		mustGit("config", c[0], c[1])
		f, err := audit(t, repo)
		if err != nil || len(f) != 1 || !strings.EqualFold(f[0], c[0]) {
			t.Errorf("%s: flagged %v (err %v)", c[0], f, err)
		}
	}
}

// Includes are refused by the Go pre-scan, before git ever parses the config.
func TestCheckLayoutRefusesIncludes(t *testing.T) {
	for _, section := range []string{"[include]\n\tpath = /tmp/x\n", "[IncludeIf \"gitdir:/\"]\n\tpath = /tmp/x\n", "[ include ]\n",
		"[extensions]\n\tWorktreeConfig = true\n"} {
		repo, _ := initRepo(t)
		f, err := os.OpenFile(filepath.Join(repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(section)
		f.Close()
		if _, err := CheckLayout(repo); !errors.Is(err, ErrUnsafeRepo) {
			t.Errorf("%q: err = %v", section, err)
		}
	}
}

func TestOpenRepoRefusesRedirectedGitDir(t *testing.T) {
	ctx := context.Background()
	elsewhere, _ := initRepo(t)
	symlinkAt := func(rel string) func(repo string) {
		return func(repo string) {
			p := filepath.Join(repo, ".git", rel)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.RemoveAll(p)
			os.Symlink(filepath.Join(elsewhere, ".git", "refs"), p)
		}
	}
	cases := map[string]func(repo string){
		"alternates": func(repo string) {
			os.WriteFile(filepath.Join(repo, ".git", "objects", "info", "alternates"), []byte(elsewhere+"/.git/objects\n"), 0o644)
		},
		"config.worktree": func(repo string) {
			os.WriteFile(filepath.Join(repo, ".git", "config.worktree"), []byte("[include]\n\tpath = /tmp/x\n"), 0o644)
		},
		"commondir": func(repo string) {
			os.WriteFile(filepath.Join(repo, ".git", "commondir"), []byte(elsewhere+"/.git\n"), 0o644)
		},
		"symlinked .git": func(repo string) {
			os.RemoveAll(filepath.Join(repo, ".git"))
			os.Symlink(filepath.Join(elsewhere, ".git"), filepath.Join(repo, ".git"))
		},
		"gitdir file": func(repo string) {
			os.RemoveAll(filepath.Join(repo, ".git"))
			os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: "+elsewhere+"/.git\n"), 0o644)
		},
		"symlinked config": func(repo string) {
			p := filepath.Join(repo, ".git", "config")
			os.Remove(p)
			os.Symlink(filepath.Join(elsewhere, ".git", "config"), p)
		},
		"symlinked refs/remotes":                  symlinkAt("refs/remotes"),
		"symlinked refs/remotes/origin/main":      symlinkAt("refs/remotes/origin/main"),
		"symlinked logs":                          symlinkAt("logs"),
		"symlinked logs/refs/remotes/origin":      symlinkAt("logs/refs/remotes/origin"),
		"symlinked logs/refs/remotes/origin/main": symlinkAt("logs/refs/remotes/origin/main"),
	}
	for name, mutate := range cases {
		repo, _ := initRepo(t)
		mutate(repo)
		if _, err := CheckLayout(repo); !errors.Is(err, ErrUnsafeRepo) {
			t.Errorf("%s: CheckLayout err = %v", name, err)
		}
		if out := (Runner{}).Push(ctx, PushSpec{RepoURL: "https://github.com/o/r", Target: "x",
			Token: "t", AppDir: repo, ExpectHost: "github.com"}); out.Reason != "unsafe_repo_config" {
			t.Errorf("%s: push = %+v", name, out)
		}
	}
	// a symlinked app dir is refused too
	link := filepath.Join(t.TempDir(), "app")
	os.Symlink(elsewhere, link)
	if _, err := CheckLayout(link); !errors.Is(err, ErrUnsafeRepo) {
		t.Errorf("symlinked app dir: %v", err)
	}
	if _, err := CheckLayout(t.TempDir()); !errors.Is(err, ErrNotRepo) {
		t.Errorf("no .git: %v", err)
	}
}

func TestOpenRepoPinsGitDir(t *testing.T) {
	repo, _ := initRepo(t)
	rc, err := openRepo(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(rc.env, "\n") + "\n"
	for _, want := range []string{"GIT_DIR=" + filepath.Join(repo, ".git") + "\n", "GIT_NO_LAZY_FETCH=1\n", "GIT_ASKPASS=\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("env missing %q", want)
		}
	}
}

func TestRoEnvStripsAskpass(t *testing.T) {
	t.Setenv("SSH_ASKPASS", "/tmp/evil")
	t.Setenv("GIT_ASKPASS", "/tmp/evil")
	env := strings.Join(roEnv(), "\n") + "\n"
	if strings.Contains(env, "/tmp/evil") || !strings.Contains(env, "GIT_ASKPASS=\n") {
		t.Errorf("roEnv = %s", env)
	}
	rc, _, cleanup, err := authEnv(repoCtx{env: roEnv()}, "", "tok", "github.com")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	n := 0
	for _, e := range rc.env {
		if strings.HasPrefix(e, "GIT_ASKPASS=") {
			n++
			if e == "GIT_ASKPASS=" {
				t.Error("authEnv kept the empty GIT_ASKPASS")
			}
		}
	}
	if n != 1 || len(rc.files) != 1 {
		t.Errorf("GIT_ASKPASS entries = %d, files = %d", n, len(rc.files))
	}
}

func TestGitVersionAtLeast(t *testing.T) {
	cases := map[string]bool{
		"git version 2.45.0\n":           true,
		"git version 2.47.2":             true,
		"git version 3.0.0":              true,
		"git version 2.44.9":             false,
		"git version 2.39.5 (Apple Git)": false,
		"garbage":                        false,
	}
	for in, want := range cases {
		if got, _ := gitVersionAtLeast(in, 2, 45); got != want {
			t.Errorf("%q: %v", in, got)
		}
	}
}

type fakeFI struct {
	os.FileInfo
	st *syscall.Stat_t
}

func (f fakeFI) Sys() any { return f.st }

func TestGitCredentialDropsToRepoOwner(t *testing.T) {
	if c := gitCredential(fakeFI{st: &syscall.Stat_t{Uid: 1000, Gid: 1000}}, 501); c != nil {
		t.Errorf("non-root must not switch uid: %+v", c)
	}
	if c := gitCredential(fakeFI{st: &syscall.Stat_t{Uid: 1000, Gid: 1001}}, 0); c == nil || c.Uid != 1000 || c.Gid != 1001 || len(c.Groups) != 0 {
		t.Errorf("root must drop to the .git owner: %+v", c)
	}
	if c := gitCredential(fakeFI{st: &syscall.Stat_t{Uid: 0, Gid: 0}}, 0); c == nil || c.Uid != sandboxUID || c.Gid != sandboxUID {
		t.Errorf("root-owned .git must drop to the sandbox uid: %+v", c)
	}
}

// fakeGitBin installs a fake git (version 2.47) that answers openRepo/audit and
// records the argv, env and fd-3 token of the final (network) invocation.
func fakeGitBin(t *testing.T) (repo, argvF, envF string) {
	t.Helper()
	dir := t.TempDir()
	repo = filepath.Join(dir, "app")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[core]\n\tbare = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	argvF, envF = filepath.Join(dir, "argv"), filepath.Join(dir, "env")
	fake := filepath.Join(dir, "git")
	script := "#!/bin/sh\ncase \"$*\" in\nversion) echo 'git version 2.47.1'; exit 0 ;;\n" +
		"*--absolute-git-dir*) cd \"$GIT_DIR\" && pwd -P; exit 0 ;;\n*--no-includes*) exit 0 ;;\nesac\n" +
		"printf '%s\\n' \"$@\" > " + argvF + "\nenv > " + envF + "\ncat <&3 > " + argvF + ".fd3\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := gitBin
	gitBin = fake
	t.Cleanup(func() { gitBin = old })
	return repo, argvF, envF
}

func assertHardened(t *testing.T, argv, env, repo string) {
	t.Helper()
	for _, kv := range hardenedConfig() {
		if kv != "-c" && !strings.Contains(argv, kv+"\n") {
			t.Errorf("argv missing override %q", kv)
		}
	}
	for _, want := range []string{"GIT_DIR=" + filepath.Join(repo, ".git"), "GIT_WORK_TREE=" + repo,
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(repo), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_LAZY_FETCH=1"} {
		if !strings.Contains(env, want+"\n") {
			t.Errorf("env missing %q", want)
		}
	}
}

// fakeGit records argv + env and exits 0, so we can assert the token never
// appears in the push command line or environment.
func TestPushTokenNotInArgvOrEnv(t *testing.T) {
	dir, argvF, envF := fakeGitBin(t)

	const token = "ghp_SUPERSECRETTOKEN"
	out := (Runner{}).Push(context.Background(), PushSpec{
		RepoURL: "https://github.com/o/r", Target: "sandboxd/x-abc",
		Token: token, AppDir: dir, ExpectHost: "github.com",
	})
	if !out.OK {
		t.Fatalf("fake push should succeed: %+v", out)
	}
	argv, _ := os.ReadFile(argvF)
	env, _ := os.ReadFile(envF)
	if strings.Contains(string(argv), token) || strings.Contains(string(env), token) {
		t.Fatal("TOKEN LEAK: token found in push argv or env")
	}
	a := string(argv)
	assertHardened(t, a, string(env), dir)
	for _, want := range []string{"push", "--no-recurse-submodules", "https://github.com/o/r", "HEAD:refs/heads/sandboxd/x-abc",
		"protocol.ext.allow=never", "protocol.file.allow=never", "core.hooksPath="} {
		if !strings.Contains(a, want) {
			t.Errorf("push argv missing %q:\n%s", want, a)
		}
	}
	if strings.Contains(a, "--force") || strings.Contains(a, "-f\n") {
		t.Error("push must never use --force")
	}
	e := string(env)
	for _, want := range []string{"GIT_ALLOW_PROTOCOL=https", "GIT_ASKPASS=/", "GIT_EXPECT_HOST=github.com"} {
		if !strings.Contains(e, want) {
			t.Errorf("push env missing %q", want)
		}
	}
	assertTokenOnFD3(t, argvF, token, e)
}

// assertTokenOnFD3 checks the token reached git only as inherited fd 3 and no token file exists.
func assertTokenOnFD3(t *testing.T, argvF, token, env string) {
	t.Helper()
	if got, _ := os.ReadFile(argvF + ".fd3"); string(got) != token {
		t.Errorf("fd 3 carried %q, want the token", got)
	}
	if strings.Contains(env, "TOKEN_FILE") {
		t.Error("a token file path is still passed to git")
	}
}

func TestClassifyPushErr(t *testing.T) {
	cases := map[string]string{
		"! [remote rejected] (shallow update not allowed)":   "shallow_push_unsupported",
		"error: failed to push some refs ... already exists": "branch_exists",
		"Updates were rejected ... (non-fast-forward)":       "non_fast_forward",
		"fatal: Authentication failed for 'https://...'":     "auth_failed",
		"fatal: unable to access ... 403":                    "auth_failed",
		"some other failure":                                 "push_failed",
	}
	for in, want := range cases {
		if got := classifyPushErr(in); got != want {
			t.Errorf("classify(%q)=%q want %q", in, got, want)
		}
	}
}

func TestHostOf(t *testing.T) {
	if HostOf("https://github.com/o/r") != "github.com" {
		t.Error("HostOf github")
	}
	if HostOf("https://github.com:443/o/r") != "github.com" {
		t.Error("HostOf strips port")
	}
}

// Only a successful parse is cached: a failed `git version` is retried next call.
func TestHostGitSupportedCachesOnlySuccess(t *testing.T) {
	dir := t.TempDir()
	fake, flag := filepath.Join(dir, "git"), filepath.Join(dir, "ok")
	script := "#!/bin/sh\n[ -f " + flag + " ] && { echo 'git version 2.47.1'; exit 0; }\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := gitBin
	gitBin = fake
	t.Cleanup(func() { gitBin = old; gitVersionOK.Delete(fake) })
	if hostGitSupported() {
		t.Fatal("failing git version must not be supported")
	}
	if _, cached := gitVersionOK.Load(fake); cached {
		t.Fatal("a failed version check was cached")
	}
	os.WriteFile(flag, nil, 0o644)
	if !hostGitSupported() {
		t.Fatal("retry after failure should succeed")
	}
	os.Remove(flag)
	if !hostGitSupported() {
		t.Error("successful parse should be cached")
	}
}

func TestCheckLayoutCoversSyncBranchRefs(t *testing.T) {
	ctx := context.Background()
	elsewhere, _ := initRepo(t)
	target := filepath.Join(elsewhere, ".git", "refs")
	cases := map[string]struct{ branch, rel string }{
		"develop ref":             {"develop", "refs/remotes/origin/develop"},
		"develop reflog":          {"develop", "logs/refs/remotes/origin/develop"},
		"release/x parent dir":    {"release/x", "refs/remotes/origin/release"},
		"release/x reflog":        {"release/x", "logs/refs/remotes/origin/release/x"},
		"release/x reflog parent": {"release/x", "logs/refs/remotes/origin/release"},
	}
	for name, c := range cases {
		repo, _ := initRepo(t)
		p := filepath.Join(repo, ".git", c.rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.RemoveAll(p)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		if _, err := CheckLayout(repo, c.branch); !errors.Is(err, ErrUnsafeRepo) {
			t.Errorf("%s: CheckLayout(%s) err = %v", name, c.branch, err)
		}
		if _, err := CheckLayout(repo, "other"); err != nil {
			t.Errorf("%s: an unrelated branch must not be refused: %v", name, err)
		}
		if out := (Runner{}).Fetch(ctx, FetchSpec{RepoURL: "https://github.com/o/r", Branch: c.branch,
			Token: "t", AppDir: repo, ExpectHost: "github.com"}); out.Reason != "unsafe_repo_config" {
			t.Errorf("%s: fetch = %+v", name, out)
		}
	}
}
