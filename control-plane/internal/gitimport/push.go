// push.go — Git B2: host-side, control-plane-only PUSH of locally-committed
// workspace changes to the app's remote. Like clone (A1) the token reaches git
// ONLY via a GIT_ASKPASS helper — here reading it from an inherited pipe fd, never
// a file — and the push reads an agent-controlled .git, so it adds defenses clone
// didn't need:
//
//   - HOST-CHECKING askpass: the token is emitted ONLY when git's credential
//     prompt resolves to the EXACT expected host (parsed from app.git_repo_url);
//     any mismatch or unparsable prompt emits nothing → token never leaves.
//   - the push target is the validated app repo URL passed on argv, NEVER the
//     repo's .git/config origin.
//   - Fetch and Push are the ONLY host git: each runs openAudited (layout check,
//     include pre-scan, GIT_DIR pin, allow-list config audit) right before the
//     network op; the caller keeps the sandbox paused across both.
//   - hardened invocation (hardenedConfig + roEnv): hooks off, https only, no
//     credential.helper/askpass/fsmonitor/alternates/proxy/lazy fetch, fsck on,
//     no auto-gc, clean HOME/config, argv-only, git ≥ 2.45, and run as the repo's
//     (sandbox) uid when root.
//
// Scope (B2 MVP): push HEAD to a NEW branch, no force, no fetch/pull, no deepen
// (a shallow-update rejection returns shallow_push_unsupported — B2.1 later).
package gitimport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// HostOf returns the bare hostname (no port) of a validated https repo URL.
func HostOf(repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// askpassScript is host-checking and CONSERVATIVE: it parses the host out of
// git's prompt and emits the username/token ONLY on an exact match. If the host
// can't be parsed, or the prompt isn't a recognized Username/Password prompt, it
// emits nothing (the token never leaves for an unexpected/rewritten host). The
// token is read from inherited fd 3 (a pipe; `<&3`, not a /dev/fd reopen, which
// fails after the uid drop), so it is never on disk.
const askpassScript = `#!/bin/sh
p="$1"
host=$(printf '%s' "$p" | sed -n "s|^[^']*'[a-zA-Z][a-zA-Z0-9+.-]*://\([^@'/]*@\)\{0,1\}\([^/:']*\).*|\2|p")
[ -n "$host" ] || exit 0
[ "$host" = "$GIT_EXPECT_HOST" ] || exit 0
case "$p" in
  Username\ *) printf '%s' "$GIT_PUSH_USER" ;;
  Password\ *) cat <&3 ;;
  *) exit 0 ;;
esac
`

// Runner runs the host-side git fetch + push over real git.
type Runner struct{}

// roEnv strips inherited GIT_*/HOME/SSH_ASKPASS, neutralizes system/global
// config, disables askpass and lazy fetch, and restricts git to https only.
func roEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GIT_") || strings.HasPrefix(e, "HOME=") || strings.HasPrefix(e, "SSH_ASKPASS=") {
			continue
		}
		env = append(env, e)
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"HOME=/nonexistent",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=https",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_ASKPASS=", // empty (non-NULL) stops git falling back to core.askPass/SSH_ASKPASS
	)
}

// hardenedConfig is the `-c` overrides on every host git; the command line
// outranks the agent-controlled .git/config.
func hardenedConfig() []string {
	var args []string
	for _, kv := range []string{
		"safe.directory=*",
		"core.fsmonitor=",
		"core.sshCommand=",
		"core.askPass=",
		"core.alternateRefsCommand=true", // alternates + this = exec during negotiation
		"core.alternateRefsPrefixes=",
		"core.logAllRefUpdates=false",
		"credential.helper=",
		"user.name=sandboxd", // reflog identity is never repo-controlled
		"user.email=sandboxd@localhost",
		"protocol.allow=never",
		"protocol.https.allow=always",
		"protocol.ext.allow=never",
		"protocol.file.allow=never",
		"http.sslVerify=true",
		"http.proxy=",
		"transfer.fsckObjects=true",
		"fetch.fsckObjects=true",
		"transfer.unpackLimit=1", // always keep a pack: no writes into objects/<xx>/
		"fetch.unpackLimit=1",
		"gc.auto=0",
		"maintenance.auto=false",
		"fetch.recurseSubmodules=false",
		"submodule.recurse=false",
	} {
		args = append(args, "-c", kv)
	}
	return args
}

// roArgs is the hardened `git -C <dir> ...` prefix.
func roArgs(appDir string, extra ...string) []string {
	base := append([]string{"-C", appDir}, hardenedConfig()...)
	return append(base, extra...)
}

// repoCtx is how host git runs against one workspace: pinned env, the uid to
// drop to, and fds inherited as 3, 4, ...
type repoCtx struct {
	env   []string
	cred  *syscall.Credential // nil = run as the current user
	files []*os.File
}

var (
	ErrNotRepo    = errors.New("not a git repo")
	ErrUnsafeRepo = errors.New("unsafe git dir")
)

// sandboxUID is the base image's sandbox user, used when .git is root-owned.
const sandboxUID = 1000

// maxConfigBytes bounds the Go-side .git/config pre-scan.
const maxConfigBytes = 1 << 20

// Includes, or worktreeConfig (git would read config.worktree and its includes), refused pre-audit.
var reIncludeSection = regexp.MustCompile(`(?i)\[\s*include|worktreeconfig`)

// layoutPaths lists the .git entries host git may read or write; every path
// component of refs/remotes/origin/<branch> and its reflog is included for each
// branch the caller is about to fetch or push (main always).
func layoutPaths(branches []string) []string {
	names := []string{"config", "HEAD", "packed-refs", "shallow", "refs", "refs/heads",
		"refs/remotes", "refs/remotes/origin", "objects", "objects/pack",
		"objects/info", "logs", "logs/refs", "logs/refs/remotes", "logs/refs/remotes/origin"}
	for _, b := range append([]string{"main"}, branches...) {
		if ValidateBranch(b) != nil {
			continue
		}
		for _, prefix := range []string{"refs/remotes/origin/", "logs/refs/remotes/origin/"} {
			acc := prefix
			for _, seg := range strings.Split(b, "/") {
				acc += seg
				names = append(names, acc)
				acc += "/"
			}
		}
	}
	return names
}

// CheckLayout is the pure-Go (no git) refusal of a .git layout that could
// redirect host git outside the workspace or pull in included config files.
// branches are the remote-tracking refs the caller will touch (see layoutPaths).
func CheckLayout(appDir string, branches ...string) (os.FileInfo, error) {
	if !filepath.IsAbs(appDir) {
		return nil, ErrNotRepo
	}
	// workspace/ and workspace/app are agent-controlled path components.
	for _, p := range []string{filepath.Dir(appDir), appDir} {
		if fi, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotRepo
		} else if err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("%w: %s", ErrUnsafeRepo, filepath.Base(p))
		}
	}
	gitDir := filepath.Join(appDir, ".git")
	fi, err := os.Lstat(gitDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotRepo
	}
	if err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: .git is not a directory", ErrUnsafeRepo)
	}
	for _, name := range layoutPaths(branches) {
		if l, err := os.Lstat(filepath.Join(gitDir, name)); err == nil && l.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: .git/%s is a symlink", ErrUnsafeRepo, name)
		}
	}
	for _, name := range []string{"objects/info/alternates", "objects/info/http-alternates", "commondir", "config.worktree"} {
		if _, err := os.Lstat(filepath.Join(gitDir, name)); err == nil {
			return nil, fmt.Errorf("%w: .git/%s present", ErrUnsafeRepo, name)
		}
	}
	if err := scanConfigIncludes(filepath.Join(gitDir, "config")); err != nil {
		return nil, err
	}
	return fi, nil
}

// scanConfigIncludes refuses [include]/[includeIf] before git ever parses the file.
func scanConfigIncludes(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("%w: .git/config unreadable", ErrUnsafeRepo)
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("%w: .git/config is not a regular file", ErrUnsafeRepo)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil || len(b) > maxConfigBytes {
		return fmt.Errorf("%w: .git/config unreadable or too large", ErrUnsafeRepo)
	}
	if reIncludeSection.Match(b) {
		return fmt.Errorf("%w: .git/config has an include section or worktreeConfig", ErrUnsafeRepo)
	}
	return nil
}

// openRepo is CheckLayout, then pins git to <appDir>/.git and verifies git agrees.
func openRepo(ctx context.Context, appDir string, branches ...string) (repoCtx, error) {
	fi, err := CheckLayout(appDir, branches...)
	if err != nil {
		return repoCtx{}, err
	}
	gitDir := filepath.Join(appDir, ".git")
	rc := repoCtx{
		env: append(roEnv(),
			"GIT_DIR="+gitDir,
			"GIT_WORK_TREE="+appDir,
			"GIT_CEILING_DIRECTORIES="+filepath.Dir(appDir),
		),
		cred: gitCredential(fi, os.Geteuid()),
	}
	out, code, err := runGit(ctx, rc, roArgs(appDir, "rev-parse", "--absolute-git-dir")...)
	if err != nil {
		return repoCtx{}, err
	}
	if code != 0 {
		return repoCtx{}, ErrNotRepo
	}
	want, err := filepath.EvalSymlinks(gitDir)
	if err != nil || strings.TrimSpace(out) != want {
		return repoCtx{}, fmt.Errorf("%w: git dir mismatch", ErrUnsafeRepo)
	}
	return rc, nil
}

// gitCredential drops a root control plane to the uid/gid owning .git (the
// sandbox user), so any residual write or exec is unprivileged.
func gitCredential(gitDir os.FileInfo, euid int) *syscall.Credential {
	st, ok := gitDir.Sys().(*syscall.Stat_t)
	if euid != 0 || !ok {
		return nil
	}
	uid, gid := uint32(st.Uid), uint32(st.Gid)
	if uid == 0 {
		uid, gid = sandboxUID, sandboxUID
	}
	return &syscall.Credential{Uid: uid, Gid: gid} // empty Groups clears supplementary groups
}

func runGit(ctx context.Context, rc repoCtx, args ...string) (out string, code int, err error) {
	return runGitTimeout(ctx, 2*time.Minute, rc, args...)
}

func runGitTimeout(ctx context.Context, d time.Duration, rc repoCtx, args ...string) (out string, code int, err error) {
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	cmd := exec.CommandContext(cctx, gitBin, args...)
	cmd.Env = rc.env
	cmd.ExtraFiles = rc.files
	if rc.cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: rc.cred}
	}
	b, e := cmd.CombinedOutput()
	if e == nil {
		return string(b), 0, nil
	}
	if ee, ok := e.(*exec.ExitError); ok {
		return string(b), ee.ExitCode(), nil
	}
	return string(b), -1, e
}

// minGitMajor/minGitMinor: GIT_NO_LAZY_FETCH needs git 2.45.
const minGitMajor, minGitMinor = 2, 45

var gitVersionOK sync.Map // gitBin -> bool; only successful parses are cached

// hostGitSupported reports whether the host git is new enough for roEnv's guards.
// Independent of the request ctx so a cancelled request can't poison the cache.
func hostGitSupported() bool {
	if ok, done := gitVersionOK.Load(gitBin); done {
		return ok.(bool)
	}
	out, code, err := runGitTimeout(context.Background(), 10*time.Second, repoCtx{env: roEnv()}, "version")
	if err != nil || code != 0 {
		return false
	}
	ok, parsed := gitVersionAtLeast(out, minGitMajor, minGitMinor)
	if parsed {
		gitVersionOK.Store(gitBin, ok)
	}
	return ok
}

var reGitVersion = regexp.MustCompile(`^git version (\d+)\.(\d+)`)

func gitVersionAtLeast(out string, major, minor int) (ok, parsed bool) {
	m := reGitVersion.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return false, false
	}
	maj, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return maj > major || maj == major && mi >= minor, true
}

// AllowedConfigKey is the allow-list of repo-local keys that clone, `git init` and the sandbox tooling write.
func AllowedConfigKey(key string) bool {
	switch key {
	case "core.repositoryformatversion", "core.filemode", "core.bare", "core.logallrefupdates",
		"core.ignorecase", "core.precomposeunicode", "core.autocrlf", "core.eol", "core.symlinks",
		"remote.origin.url", "remote.origin.fetch", "extensions.objectformat",
		"user.name", "user.email", "init.defaultbranch", "pull.rebase":
		return true
	}
	if strings.HasPrefix(key, "advice.") {
		return true
	}
	if name, ok := strings.CutPrefix(key, "branch."); ok {
		for _, suf := range []string{".remote", ".merge"} {
			if n, ok := strings.CutSuffix(name, suf); ok && n != "" {
				return true
			}
		}
	}
	return false
}

// FlaggedConfigKeys returns the keys of `git config --list -z` output outside the allow-list.
func FlaggedConfigKeys(listZ string) []string {
	var flagged []string
	for _, rec := range strings.Split(listZ, "\x00") {
		if rec == "" {
			continue
		}
		key, _, _ := strings.Cut(rec, "\n")
		if !AllowedConfigKey(key) {
			flagged = append(flagged, key)
		}
	}
	return flagged
}

func auditConfig(ctx context.Context, rc repoCtx, appDir string) ([]string, error) {
	// --no-includes: an include (already refused by the pre-scan) would make git read the included file.
	out, code, err := runGit(ctx, rc, roArgs(appDir, "config", "--local", "--no-includes", "--list", "-z")...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return []string{"config unreadable"}, nil
	}
	return FlaggedConfigKeys(out), nil
}

// RepoState is the local git state the push pre-flight needs (queried in the sandbox).
type RepoState struct {
	IsRepo        bool
	HasHEAD       bool
	Detached      bool
	IsShallow     bool
	CurrentBranch string
	HeadShort     string
}

// PushSpec is one push request. Token is the DECRYPTED PAT (never logged).
type PushSpec struct {
	RepoURL    string // validated https, tokenless
	Target     string // branch name to push to (validated ref); git/push passes a NEW branch, git/sync the sync branch
	Rev        string // commit SHA to push (ValidCommitSHA); "" = HEAD
	Username   string // credential username; defaults x-access-token
	Token      string // decrypted PAT
	AppDir     string // host workspace path
	ExpectHost string // host the askpass will release the token to
}

// PushOutcome is the classified result. Output is sanitized (token-redacted) and
// must not be logged by default (it can echo repo content).
type PushOutcome struct {
	OK     bool
	Reason string // branch_exists|non_fast_forward|auth_failed|shallow_push_unsupported|unsafe_repo_config|host_git_too_old|push_failed
	Output string
}

// Push pushes Rev (or HEAD) to refs/heads/<Target> at the validated URL,
// tokenless, with the host-checking askpass. No --force, no deepen.
func (Runner) Push(ctx context.Context, spec PushSpec) PushOutcome {
	if ValidateRepoURL(spec.RepoURL) != nil || ValidateBranch(spec.Target) != nil ||
		(spec.Rev != "" && !ValidCommitSHA(spec.Rev)) || spec.Token == "" || spec.ExpectHost == "" {
		return PushOutcome{Reason: "push_failed"}
	}
	rc, reason := openAudited(ctx, spec.AppDir, spec.Target)
	if reason != "" {
		if reason == "failed" {
			reason = "push_failed"
		}
		return PushOutcome{Reason: reason}
	}
	rc, hooksDir, cleanup, err := authEnv(rc, spec.Username, spec.Token, spec.ExpectHost)
	if err != nil {
		return PushOutcome{Reason: "push_failed"}
	}
	defer cleanup()
	out, code, runErr := runGit(ctx, rc, pushArgs(spec, hooksDir)...)
	msg := sanitize(out, spec.Token)
	if runErr == nil && code == 0 {
		return PushOutcome{OK: true, Output: msg}
	}
	return PushOutcome{Reason: classifyPushErr(out), Output: msg}
}

func pushArgs(spec PushSpec, hooksDir string) []string {
	rev := spec.Rev
	if rev == "" {
		rev = "HEAD"
	}
	return roArgs(spec.AppDir, "-c", "core.hooksPath="+hooksDir,
		"push", "--no-recurse-submodules", spec.RepoURL, rev+":refs/heads/"+spec.Target)
}

var reCommitSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ValidCommitSHA reports a full SHA-1/SHA-256 hex object name (safe on argv).
func ValidCommitSHA(s string) bool { return reCommitSHA.MatchString(s) }

// openAudited is openRepo + the allow-list audit + the git version gate right
// before a network op; reason is "" (ok), "unsafe_repo_config",
// "host_git_too_old" or "failed".
func openAudited(ctx context.Context, appDir string, branches ...string) (repoCtx, string) {
	rc, err := openRepo(ctx, appDir, branches...)
	if errors.Is(err, ErrUnsafeRepo) {
		return rc, "unsafe_repo_config"
	}
	if err != nil {
		return rc, "failed"
	}
	flagged, err := auditConfig(ctx, rc, appDir)
	if err != nil {
		return rc, "failed"
	}
	if len(flagged) > 0 {
		return rc, "unsafe_repo_config"
	}
	// After the read-only checks (no lazy fetch there), before anything that talks to a remote.
	if !hostGitSupported() {
		return rc, "host_git_too_old"
	}
	return rc, ""
}

// authEnv adds the host-checking askpass and an empty hooks dir to rc; the
// token goes to git as inherited fd 3 (a pipe), never a file. cleanup closes
// the pipe and removes the temp dir.
func authEnv(rc repoCtx, username, token, expectHost string) (out repoCtx, hooksDir string, cleanup func(), err error) {
	if username == "" {
		username = "x-access-token"
	}
	tmp, err := os.MkdirTemp("", "gitpush-")
	if err != nil {
		return rc, "", nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		os.RemoveAll(tmp)
		return rc, "", nil, err
	}
	cleanup = func() { pr.Close(); os.RemoveAll(tmp) }
	hooksDir = tmp + "/hooks" // empty dir => hooks disabled
	askpass := tmp + "/askpass.sh"
	// A PAT is far below the pipe buffer, so this write never blocks.
	_, err = io.WriteString(pw, token)
	if cerr := pw.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if err = os.Mkdir(hooksDir, 0o700); err == nil {
			err = os.WriteFile(askpass, []byte(askpassScript), 0o700)
		}
	}
	if err == nil && rc.cred != nil {
		for _, p := range []string{tmp, hooksDir, askpass} {
			if err = os.Lchown(p, int(rc.cred.Uid), int(rc.cred.Gid)); err != nil {
				break
			}
		}
	}
	if err != nil {
		cleanup()
		return rc, "", nil, err
	}
	var env []string
	for _, e := range rc.env {
		if !strings.HasPrefix(e, "GIT_ASKPASS=") {
			env = append(env, e)
		}
	}
	rc.env = append(env,
		"GIT_ASKPASS="+askpass,
		"GIT_PUSH_USER="+username,     // not secret
		"GIT_EXPECT_HOST="+expectHost, // exact-host gate in askpass
	)
	rc.files = []*os.File{pr} // fd 3 in git and its children
	return rc, hooksDir, cleanup, nil
}

func classifyPushErr(out string) string {
	o := strings.ToLower(out)
	switch {
	case strings.Contains(o, "shallow update not allowed"):
		return "shallow_push_unsupported"
	case strings.Contains(o, "already exists"):
		return "branch_exists"
	case strings.Contains(o, "non-fast-forward"), strings.Contains(o, "fetch first"):
		return "non_fast_forward"
	case strings.Contains(o, "authentication failed"),
		strings.Contains(o, "could not read username"),
		strings.Contains(o, "could not read password"),
		strings.Contains(o, "403"):
		return "auth_failed"
	}
	return "push_failed"
}
