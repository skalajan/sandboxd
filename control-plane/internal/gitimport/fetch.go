// fetch.go — host-side FETCH of the app's remote sync branch into refs/remotes/origin/<branch>,
// with the same hardening as Push (openRepo + allow-list audit, hardenedConfig,
// tokenless URL on argv, host-checking askpass, sandbox uid).
package gitimport

import (
	"context"
	"strings"
	"time"
)

// FetchSpec is one fetch request. Token is the DECRYPTED PAT (never logged).
type FetchSpec struct {
	RepoURL    string // validated https, tokenless
	Branch     string // remote branch to fetch, e.g. "main" (ValidateBranch)
	Username   string // defaults x-access-token
	Token      string // decrypted PAT
	AppDir     string
	ExpectHost string
	Unshallow  bool // add --unshallow (caller sets when RepoState.IsShallow)
}

// FetchOutcome is the classified result; Output is sanitized.
type FetchOutcome struct {
	OK     bool
	Reason string // no_main_branch|auth_failed|unsafe_repo_config|host_git_too_old|fetch_failed
	Output string
}

// Fetch updates refs/remotes/origin/<Branch> from the validated URL, tokenless.
func (Runner) Fetch(ctx context.Context, spec FetchSpec) FetchOutcome {
	if ValidateRepoURL(spec.RepoURL) != nil || ValidateBranch(spec.Branch) != nil ||
		spec.Token == "" || spec.ExpectHost == "" {
		return FetchOutcome{Reason: "fetch_failed"}
	}
	rc, reason := openAudited(ctx, spec.AppDir, spec.Branch)
	if reason != "" {
		if reason == "failed" {
			reason = "fetch_failed"
		}
		return FetchOutcome{Reason: reason}
	}
	rc, hooksDir, cleanup, err := authEnv(rc, spec.Username, spec.Token, spec.ExpectHost)
	if err != nil {
		return FetchOutcome{Reason: "fetch_failed"}
	}
	defer cleanup()
	out, code, runErr := runGitTimeout(ctx, 5*time.Minute, rc, fetchArgs(spec, hooksDir)...)
	msg := sanitize(out, spec.Token)
	if runErr == nil && code == 0 {
		return FetchOutcome{OK: true, Output: msg}
	}
	return FetchOutcome{Reason: classifyFetchErr(out), Output: msg}
}

func fetchArgs(spec FetchSpec, hooksDir string) []string {
	args := roArgs(spec.AppDir, "-c", "core.hooksPath="+hooksDir, "fetch")
	if spec.Unshallow {
		args = append(args, "--unshallow")
	}
	return append(args, "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head",
		"--no-auto-maintenance", "--no-auto-gc", spec.RepoURL,
		"+refs/heads/"+spec.Branch+":refs/remotes/origin/"+spec.Branch)
}

func classifyFetchErr(out string) string {
	if strings.Contains(strings.ToLower(out), "couldn't find remote ref") {
		return "no_main_branch"
	}
	if classifyPushErr(out) == "auth_failed" {
		return "auth_failed"
	}
	return "fetch_failed"
}
