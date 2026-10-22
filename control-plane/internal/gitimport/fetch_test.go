package gitimport

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestClassifyFetchErr(t *testing.T) {
	cases := map[string]string{
		"fatal: couldn't find remote ref refs/heads/main": "no_main_branch",
		"fatal: Authentication failed for 'https://...'":  "auth_failed",
		"fatal: could not read Username for 'https://x'":  "auth_failed",
		"fatal: unable to access ... 403":                 "auth_failed",
		"some other failure":                              "fetch_failed",
	}
	for in, want := range cases {
		if got := classifyFetchErr(in); got != want {
			t.Errorf("classify(%q)=%q want %q", in, got, want)
		}
	}
}

// file:// is blocked by GIT_ALLOW_PROTOCOL=https, so a fake git asserts argv/env instead.
func TestFetchArgvAndTokenHygiene(t *testing.T) {
	dir, argvF, envF := fakeGitBin(t)

	const token = "ghp_SUPERSECRETTOKEN"
	out := (Runner{}).Fetch(context.Background(), FetchSpec{
		RepoURL: "https://github.com/o/r", Branch: "main", Token: token,
		AppDir: dir, ExpectHost: "github.com", Unshallow: true,
	})
	if !out.OK {
		t.Fatalf("fake fetch should succeed: %+v", out)
	}
	argv, _ := os.ReadFile(argvF)
	env, _ := os.ReadFile(envF)
	if strings.Contains(string(argv), token) || strings.Contains(string(env), token) {
		t.Fatal("TOKEN LEAK: token found in fetch argv or env")
	}
	a := string(argv)
	assertHardened(t, a, string(env), dir)
	for _, want := range []string{"fetch", "--unshallow", "--no-tags", "--no-recurse-submodules",
		"--no-write-fetch-head", "--no-auto-maintenance", "--no-auto-gc",
		"https://github.com/o/r", "+refs/heads/main:refs/remotes/origin/main", "core.hooksPath="} {
		if !strings.Contains(a, want) {
			t.Errorf("fetch argv missing %q:\n%s", want, a)
		}
	}
	for _, want := range []string{"GIT_ALLOW_PROTOCOL=https", "GIT_ASKPASS=/", "GIT_EXPECT_HOST=github.com"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("fetch env missing %q", want)
		}
	}
	assertTokenOnFD3(t, argvF, token, string(env))
}

func TestFetchNoUnshallowByDefaultAndValidation(t *testing.T) {
	a := strings.Join(fetchArgs(FetchSpec{RepoURL: "https://github.com/o/r", Branch: "main", AppDir: "/x"}, "/h"), " ")
	if strings.Contains(a, "--unshallow") {
		t.Error("unexpected --unshallow")
	}
	out := (Runner{}).Fetch(context.Background(), FetchSpec{RepoURL: "http://github.com/o/r", Branch: "main", Token: "t", ExpectHost: "github.com"})
	if out.OK || out.Reason != "fetch_failed" {
		t.Errorf("invalid url should fail: %+v", out)
	}
}

// Fetch audits itself, so an unsafe config is refused even if the caller skipped the preflight.
func TestFetchRefusesUnsafeRepo(t *testing.T) {
	repo, mustGit := initRepo(t)
	mustGit("config", "include.path", "/tmp/x")
	out := (Runner{}).Fetch(context.Background(), FetchSpec{RepoURL: "https://github.com/o/r", Branch: "main",
		Token: "t", AppDir: repo, ExpectHost: "github.com"})
	if out.OK || out.Reason != "unsafe_repo_config" {
		t.Errorf("fetch = %+v", out)
	}
}
