// v1_git_push.go — Git B2: push locally-committed workspace changes to the app's
// remote. The network push is host-side (the token is decrypted HERE and reaches
// git only via gitimport's host-checking askpass) with the sandbox paused; repo
// state/unpushed/audit queries run in the (woken) sandbox. The push
// target comes from app.git_repo_url + app.git_credential_id metadata — NEVER the
// repo's .git/config origin. New branch only, no force, no fetch/pull, no deepen.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/events"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/gitimport"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// pushRunner is the ENTIRE host-side git surface: push + sync fetch, each run
// inside withPaused (real impl = gitimport.Runner; faked in tests). Every
// read-only query runs in the sandbox instead.
type pushRunner interface {
	Push(ctx context.Context, spec gitimport.PushSpec) gitimport.PushOutcome
	Fetch(ctx context.Context, spec gitimport.FetchSpec) gitimport.FetchOutcome
}

func (s *Server) pusher() pushRunner {
	if s.GitPush != nil {
		return s.GitPush
	}
	return gitimport.Runner{}
}

// sandboxPauser freezes a sandbox container so the agent can't rewrite .git
// between the host-side audit and the network op (real impl = *docker.Client).
type sandboxPauser interface {
	Pause(ctx context.Context, name string) error
	Unpause(ctx context.Context, name string) error
}

func (s *Server) gitPauser() sandboxPauser {
	if s.GitPause != nil {
		return s.GitPause
	}
	return s.Docker
}

// withPaused runs fn with the sandbox frozen (fn must not exec into it); unpause
// always runs, even after the request is cancelled.
func (s *Server) withPaused(r *http.Request, sbID string, fn func()) error {
	p, name := s.gitPauser(), "s-"+sbID
	if err := p.Pause(r.Context(), name); err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		defer cancel()
		if err := p.Unpause(ctx, name); err != nil {
			s.loggerFor(r, sbID).Error("git: unpause sandbox failed", "err", err.Error())
		}
	}()
	fn()
	return nil
}

type v1GitPushReq struct {
	Branch string `json:"branch"`
}

type v1GitPushResp struct {
	Pushed       bool   `json:"pushed"`
	Reason       string `json:"reason,omitempty"`
	Branch       string `json:"branch,omitempty"`
	RemoteURL    string `json:"remote_url,omitempty"` // tokenless
	Commits      int    `json:"commits,omitempty"`
	HeadDetached bool   `json:"head_detached,omitempty"`
}

// reservedDefaultBranches are remote default-branch names we refuse to push to in
// B2 (new-branch only). develop/trunk are NOT reserved unless they equal the
// import branch (handled separately).
var reservedDefaultBranches = map[string]bool{"main": true, "master": true}

// POST /v1/apps/{id}/git/push
func (s *Server) v1GitPush(w http.ResponseWriter, r *http.Request) {
	app, err := s.Store.GetAppForOwner(r.Context(), r.PathValue("id"), tenantToken(r))
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such app")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "lookup failed")
		return
	}
	var req v1GitPushReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}

	unavail := func(reason string) { writeJSON(w, http.StatusOK, v1GitPushResp{Pushed: false, Reason: reason}) }

	// 1. Git metadata is the source of truth (never .git/config origin).
	repoURL, reason := gitRemoteOf(app)
	if reason != "" {
		unavail(reason)
		return
	}
	importBranch := syncBranchFor(app)

	// 2. Workspace + a running sandbox (woken if stopped): repo queries run in
	// it, and the host-side push runs with it paused.
	sb, err := s.Store.CurrentSandboxForApp(r.Context(), app.ID)
	if err != nil {
		unavail("no_workspace")
		return
	}
	if sb, err = s.wakeForGit(w, r, sb); err != nil {
		return
	}
	if sb.Status != "running" {
		unavail("sandbox_not_running")
		return
	}
	// Acquire the SAME per-workspace git mutation lock as commit, so a push can't
	// overlap a commit (which would let it publish a stale HEAD). Everything
	// below — RepoState/HEAD, audit, unpushed, and the push itself — runs under
	// the lock, so the HEAD we push is the one we evaluated.
	s.Locks.Lock(gitLockKey(sb.ID))
	defer s.Locks.Unlock(gitLockKey(sb.ID))
	appDir := filepath.Join(sb.WorkspaceMnt, "workspace", "app")
	runner, ex := s.pusher(), s.gitExec()

	// 3. Repo state + UNSAFE-CONFIG audit (before any network/credential use).
	state, reason := gitRepoPreflight(r.Context(), ex, sb.ID, appDir, importBranch, "push_failed")
	if reason != "" {
		unavail(reason)
		return
	}
	if gitInProgress(appDir) != "" {
		unavail("merge_in_progress")
		return
	}

	// 4. Target branch (NEW branch only; reject import + main/master).
	target := strings.TrimSpace(req.Branch)
	if target == "" {
		target = defaultPushBranch(app.Name, state.HeadShort)
	}
	if gitimport.ValidateBranch(target) != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid branch name")
		return
	}
	if strings.EqualFold(target, importBranch) || reservedDefaultBranches[strings.ToLower(target)] {
		unavail("refuses_default_branch")
		return
	}

	// 5. Unpushed commits (no network). Conservative when baseline is absent.
	commits, err := gitUnpushed(r.Context(), ex, sb.ID, importBranch)
	if err != nil {
		unavail("push_failed")
		return
	}
	if commits == 0 {
		unavail("no_local_commits")
		return
	}

	// 6. Decrypt the owner-scoped credential — ONLY here, in the push path.
	if s.Secrets == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "credential store not configured")
		return
	}
	token, reason := s.openGitToken(r, app.GitCredentialID.String, "push_failed")
	if reason != "" {
		unavail(reason)
		return
	}

	// 7. Push (host-checking askpass; tokenless URL; no force; no deepen), with
	// the sandbox frozen so Push's own audit still holds at the network op.
	var outcome gitimport.PushOutcome
	if err := s.withPaused(r, sb.ID, func() {
		outcome = runner.Push(r.Context(), gitimport.PushSpec{
			RepoURL:    repoURL,
			Target:     target,
			Token:      string(token),
			AppDir:     appDir,
			ExpectHost: gitimport.HostOf(repoURL),
		})
	}); err != nil {
		outcome = gitimport.PushOutcome{Reason: "push_failed"}
	}
	for i := range token { // best-effort scrub
		token[i] = 0
	}

	if !outcome.OK {
		s.auditAction(r, audit.Entry{Action: "git.push.failed", Target: app.ID})
		s.recordEvent(r, events.Event{Type: events.GitRepoPushFailed, Severity: events.SeverityWarning,
			Message: "Git push failed", AppID: app.ID, SandboxID: sb.ID,
			Payload: map[string]any{"repo_url": repoURL, "branch": target, "reason": outcome.Reason}})
		unavail(outcome.Reason)
		return
	}
	s.auditAction(r, audit.Entry{Action: "git.push", Target: app.ID})
	s.recordEvent(r, events.Event{Type: events.GitRepoPushed, Severity: events.SeverityInfo,
		Message: "Pushed to " + target, AppID: app.ID, SandboxID: sb.ID,
		Payload: map[string]any{"repo_url": repoURL, "branch": target, "commits": commits}})
	writeJSON(w, http.StatusOK, v1GitPushResp{
		Pushed: true, Branch: target, RemoteURL: repoURL, Commits: commits, HeadDetached: state.Detached,
	})
}

// gitRemoteOf returns the app's validated tokenless repo URL; reason "" = ok
// (a credential must also be associated).
func gitRemoteOf(app *store.App) (repoURL, reason string) {
	if !app.GitRepoURL.Valid || app.GitRepoURL.String == "" || gitimport.ValidateRepoURL(app.GitRepoURL.String) != nil {
		return "", "no_repo_url"
	}
	if !app.GitCredentialID.Valid || app.GitCredentialID.String == "" {
		return "", "no_credential"
	}
	return app.GitRepoURL.String, ""
}

// gitRepoPreflight checks the host-side .git layout (pure Go, no git), then
// repo state + the allow-list audit IN THE SANDBOX; reason "" = ok, failReason
// on an internal error. The sandbox answers are untrusted: they can only make
// us refuse — Fetch/Push re-audit host-side before any network op.
func gitRepoPreflight(ctx context.Context, ex sandboxExecer, sbID, appDir, branch, failReason string) (gitimport.RepoState, string) {
	var state gitimport.RepoState
	if _, err := gitimport.CheckLayout(appDir, branch); errors.Is(err, gitimport.ErrNotRepo) {
		return state, "not_a_git_repo"
	} else if err != nil {
		return state, "unsafe_repo_config"
	}
	state, err := gitRepoState(ctx, ex, sbID)
	if err != nil {
		return state, failReason
	}
	if !state.IsRepo {
		return state, "not_a_git_repo"
	}
	if !state.HasHEAD {
		return state, "empty_repo_unsupported"
	}
	res, err := ex.Exec(ctx, "s-"+sbID, gitArgs("config", "--local", "--no-includes", "--list", "-z"))
	if err != nil {
		return state, failReason
	}
	if res.ExitCode != 0 || len(gitimport.FlaggedConfigKeys(res.Stdout)) > 0 {
		return state, "unsafe_repo_config"
	}
	return state, ""
}

var reShortSHA = regexp.MustCompile(`^[0-9a-f]{4,64}$`)

// gitRepoState queries the repo state in the sandbox (refs/booleans only).
func gitRepoState(ctx context.Context, ex sandboxExecer, sbID string) (gitimport.RepoState, error) {
	var st gitimport.RepoState
	run := func(args ...string) (string, bool, error) {
		res, err := ex.Exec(ctx, "s-"+sbID, gitArgs(args...))
		if err != nil {
			return "", false, err
		}
		return strings.TrimSpace(res.Stdout), res.ExitCode == 0, nil
	}
	out, ok, err := run("rev-parse", "--is-inside-work-tree")
	if err != nil {
		return st, err
	}
	if !ok || out != "true" {
		return st, nil
	}
	st.IsRepo = true
	if _, ok, _ := run("rev-parse", "--verify", "HEAD"); ok {
		st.HasHEAD = true
	}
	if o, ok, _ := run("rev-parse", "--abbrev-ref", "HEAD"); ok {
		if o == "HEAD" {
			st.Detached = true
		} else {
			st.CurrentBranch = o
		}
	}
	if o, ok, _ := run("rev-parse", "--short", "HEAD"); ok && reShortSHA.MatchString(o) {
		st.HeadShort = o
	}
	if o, ok, _ := run("rev-parse", "--is-shallow-repository"); ok {
		st.IsShallow = o == "true"
	}
	return st, nil
}

// gitUnpushed counts commits on HEAD beyond refs/remotes/origin/<branch>, or
// all of HEAD when that baseline is absent (reset .git / fork), in the sandbox.
func gitUnpushed(ctx context.Context, ex sandboxExecer, sbID, branch string) (int, error) {
	rng := "HEAD"
	baseline := "refs/remotes/origin/" + branch
	if res, err := ex.Exec(ctx, "s-"+sbID, gitArgs("rev-parse", "--verify", baseline)); err == nil && res.ExitCode == 0 {
		rng = baseline + "..HEAD"
	}
	res, err := ex.Exec(ctx, "s-"+sbID, gitArgs("rev-list", "--count", rng))
	if err != nil {
		return 0, err
	}
	if res.ExitCode != 0 {
		return 0, errors.New("rev-list failed")
	}
	return atoiTrim(res.Stdout), nil
}

// openGitToken decrypts the owner-scoped credential (caller checks s.Secrets and
// scrubs the token after use); reason "" = ok.
func (s *Server) openGitToken(r *http.Request, credID, failReason string) ([]byte, string) {
	enc, nonce, found, err := s.Store.GetGitCredentialSecret(r.Context(), tenantToken(r), credID)
	if err != nil {
		return nil, failReason
	}
	if !found {
		return nil, "credential_not_found"
	}
	token, err := s.Secrets.Open(enc, nonce)
	if err != nil {
		return nil, failReason
	}
	return token, ""
}

// defaultPushBranch builds a safe NEW branch name: sandboxd/<slug>-<shortsha>.
func defaultPushBranch(appName, headShort string) string {
	slug := slugify(appName)
	if slug == "" {
		slug = "app"
	}
	if headShort == "" {
		headShort = "work"
	}
	return "sandboxd/" + slug + "-" + headShort
}

// slugify lowercases and keeps [a-z0-9-], collapsing the rest to single dashes.
func slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
		if b.Len() >= 32 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}
