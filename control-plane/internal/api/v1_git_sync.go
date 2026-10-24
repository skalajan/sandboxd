// v1_git_sync.go — Publish / Rebase on the sync branch: auto-commit user files, fetch the
// remote sync branch HOST-side (token stays in the control plane), then merge or rebase
// IN THE SANDBOX (repo-controlled merge drivers/filters/hooks must never run on
// the control plane). Publish pushes HEAD to the sync branch fast-forward only. A conflict
// is left in progress and handed to an agent task; that result is never pushed
// automatically — the user's next Publish does it.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/events"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/gitimport"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// defaultSyncBranch applies when neither the app's import branch nor
// SANDBOXD_SYNC_BRANCH names one.
const defaultSyncBranch = "main"

// syncBranchFor is the remote branch git/sync integrates with and publishes to:
// the app's import branch, else SANDBOXD_SYNC_BRANCH, else main. Both sources
// pass ValidateBranch (the import branch at app creation), so the value is safe in argv.
func syncBranchFor(app *store.App) string {
	if app != nil && app.GitBranch.Valid && app.GitBranch.String != "" {
		return app.GitBranch.String
	}
	if b := strings.TrimSpace(os.Getenv("SANDBOXD_SYNC_BRANCH")); b != "" && gitimport.ValidateBranch(b) == nil {
		return b
	}
	return defaultSyncBranch
}

func syncRemoteRef(branch string) string { return "refs/remotes/origin/" + branch }

type v1GitSyncReq struct {
	Mode string `json:"mode"` // publish|rebase
}

type v1GitSyncResp struct {
	State         string   `json:"state"`            // published|rebased|up_to_date|resolving|failed
	Branch        string   `json:"branch,omitempty"` // remote branch the sync targets
	Reason        string   `json:"reason,omitempty"`
	Commits       int      `json:"commits,omitempty"`
	AutoCommitted string   `json:"auto_committed,omitempty"`
	TaskID        string   `json:"task_id,omitempty"`
	Conflicts     []string `json:"conflicts,omitempty"`
	Operation     string   `json:"operation,omitempty"` // merge|rebase
}

func syncFailed(reason string) v1GitSyncResp { return v1GitSyncResp{State: "failed", Reason: reason} }

// POST /v1/apps/{id}/git/sync
func (s *Server) v1GitSync(w http.ResponseWriter, r *http.Request) {
	app, err := s.Store.GetAppForOwner(r.Context(), r.PathValue("id"), tenantToken(r))
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such app")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "lookup failed")
		return
	}
	var req v1GitSyncReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Mode != "publish" && req.Mode != "rebase") {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", `mode must be "publish" or "rebase"`)
		return
	}
	branch := syncBranchFor(app)
	reply := func(resp v1GitSyncResp) { resp.Branch = branch; writeJSON(w, http.StatusOK, resp) }

	repoURL, reason := gitRemoteOf(app)
	if reason != "" {
		reply(syncFailed(reason))
		return
	}
	sb, err := s.Store.CurrentSandboxForApp(r.Context(), app.ID)
	if err != nil {
		reply(syncFailed("no_workspace"))
		return
	}
	if sb, err = s.wakeForGit(w, r, sb); err != nil {
		return
	}
	if sb.Status != "running" {
		reply(syncFailed("sandbox_not_running"))
		return
	}

	lockKey := gitLockKey(sb.ID)
	s.Locks.Lock(lockKey)
	locked := true
	defer func() {
		if locked {
			s.Locks.Unlock(lockKey)
		}
	}()
	appDir := filepath.Join(sb.WorkspaceMnt, "workspace", "app")

	state, reason := gitRepoPreflight(r.Context(), s.gitExec(), sb.ID, appDir, branch, "sync_failed")
	if reason != "" {
		reply(syncFailed(reason))
		return
	}
	// Idempotent: never start a second merge/rebase or a second agent.
	if op := gitInProgress(appDir); op != "" {
		taskID, _ := s.Store.GetAppGitResolveTaskID(r.Context(), app.ID)
		reply(v1GitSyncResp{State: "resolving", Operation: op, TaskID: taskID,
			Conflicts: gitConflicts(r.Context(), s.gitExec(), sb.ID)})
		return
	}

	if s.Secrets == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "credential store not configured")
		return
	}
	token, reason := s.openGitToken(r, app.GitCredentialID.String, "sync_failed")
	if reason != "" {
		reply(syncFailed(reason))
		return
	}
	resp, resolveOp := s.gitSyncLocked(r, sb, req.Mode, repoURL, appDir, branch, state, string(token))
	for i := range token { // best-effort scrub
		token[i] = 0
	}
	if resolveOp != "" {
		// Release the lock before the agent runs so it never blocks commit/push.
		s.Locks.Unlock(lockKey)
		locked = false
		resp.TaskID = s.submitResolveTask(r, sb.ID, resolveOp, branch)
		if resp.TaskID != "" {
			if err := s.Store.SetAppGitResolveTaskID(r.Context(), app.ID, resp.TaskID); err != nil {
				s.loggerFor(r, sb.ID).Warn("git sync: record resolve task failed", "err", err.Error())
			}
		} else {
			s.Locks.Lock(lockKey)
			locked = true
			if gitInProgress(appDir) == resolveOp {
				gitAbort(r.Context(), s.gitExec(), sb.ID, resolveOp)
			}
			resp.State, resp.Reason, resp.Operation = "failed", "agent_unavailable", ""
		}
	}

	ev := events.Event{AppID: app.ID, SandboxID: sb.ID, Severity: events.SeverityInfo,
		Payload: map[string]any{"repo_url": repoURL, "mode": req.Mode, "branch": branch, "reason": resp.Reason}}
	switch resp.State {
	case "published", "rebased", "up_to_date":
		if err := s.Store.SetAppGitResolveTaskID(r.Context(), app.ID, ""); err != nil {
			s.loggerFor(r, sb.ID).Warn("git sync: clear resolve task failed", "err", err.Error())
		}
		if resp.State == "published" {
			ev.Type, ev.Message = events.GitSyncPublished, "Published to "+branch
		} else if resp.State == "rebased" {
			ev.Type, ev.Message = events.GitSyncRebased, "Rebased onto "+branch
		}
	case "resolving":
		ev.Type, ev.Message, ev.TaskID = events.GitSyncConflict, "Agent resolving "+resp.Operation+" conflicts", resp.TaskID
		ev.Payload["files"] = len(resp.Conflicts)
	case "failed":
		ev.Type, ev.Message, ev.Severity = events.GitSyncFailed, "Git sync failed", events.SeverityWarning
	}
	if ev.Type != "" {
		s.auditAction(r, audit.Entry{Action: ev.Type, Target: app.ID, Detail: map[string]any{"mode": req.Mode}})
		s.recordEvent(r, ev)
	}
	reply(resp)
}

// gitSyncLocked runs auto-commit → fetch → integrate → (publish) push. Caller
// holds the git lock and scrubs token. A non-empty resolveOp means a conflicted
// merge|rebase is left in progress for the caller to hand to an agent.
func (s *Server) gitSyncLocked(r *http.Request, sb *store.Sandbox, mode, repoURL, appDir, branch string,
	state gitimport.RepoState, token string) (resp v1GitSyncResp, resolveOp string) {
	ctx, ex, runner := r.Context(), s.gitExec(), s.pusher()
	remoteRef := syncRemoteRef(branch)

	// 1. Auto-commit user files (runtime files excluded by gitCommit's default).
	auto := ""
	c := gitCommit(ctx, ex, sb.ID, v1GitCommitReq{Message: "sandboxd: auto-commit before " + mode})
	if c.Committed {
		auto = c.SHA
		if len(auto) > 7 {
			auto = auto[:7]
		}
	} else if c.Reason != "no_changes" {
		return syncFailed("sync_failed"), ""
	}
	withAuto := func(resp v1GitSyncResp) v1GitSyncResp { resp.AutoCommitted = auto; return resp }

	// 2. Host-side fetch of the remote sync branch (also moves the Unpushed baseline),
	// with the sandbox frozen so Fetch's own audit still holds at the network op.
	var f gitimport.FetchOutcome
	if err := s.withPaused(r, sb.ID, func() {
		f = runner.Fetch(ctx, gitimport.FetchSpec{
			RepoURL: repoURL, Branch: branch, Token: token, AppDir: appDir,
			ExpectHost: gitimport.HostOf(repoURL), Unshallow: state.IsShallow,
		})
	}); err != nil {
		return withAuto(syncFailed("sync_failed")), ""
	}
	if !f.OK {
		if f.Reason == "" {
			f.Reason = "fetch_failed"
		}
		return withAuto(syncFailed(f.Reason)), ""
	}
	b, err := ex.Exec(ctx, "s-"+sb.ID, gitArgs("rev-list", "--count", "HEAD.."+remoteRef))
	if err != nil || b.ExitCode != 0 {
		return withAuto(syncFailed("sync_failed")), ""
	}
	behind := atoiTrim(b.Stdout)

	// 3. Integrate IN THE SANDBOX.
	if behind > 0 {
		args := syncGitArgs("merge", "--no-edit", "--no-verify", remoteRef)
		if mode == "rebase" {
			args = syncGitArgs("rebase", "--no-verify", "--autostash", remoteRef)
		}
		res, err := ex.Exec(ctx, "s-"+sb.ID, args)
		if err != nil {
			return withAuto(syncFailed("sync_failed")), ""
		}
		if res.ExitCode != 0 {
			inProg := gitInProgress(appDir)
			conflicts := []string{}
			if inProg != "" {
				conflicts = gitConflicts(ctx, ex, sb.ID)
			}
			if inProg == "" || len(conflicts) == 0 {
				if inProg != "" {
					gitAbort(ctx, ex, sb.ID, inProg)
				}
				return withAuto(syncFailed("sync_failed")), ""
			}
			return withAuto(v1GitSyncResp{State: "resolving", Operation: inProg, Conflicts: conflicts}), inProg
		}
	}

	// 4. Outcome.
	commits, err := gitUnpushed(ctx, ex, sb.ID, branch)
	if err != nil {
		return withAuto(syncFailed("sync_failed")), ""
	}
	if mode == "rebase" {
		if behind == 0 {
			return withAuto(v1GitSyncResp{State: "up_to_date"}), ""
		}
		return withAuto(v1GitSyncResp{State: "rebased", Commits: commits}), ""
	}
	if commits == 0 {
		return withAuto(v1GitSyncResp{State: "up_to_date"}), ""
	}
	// Push a pinned SHA (not HEAD) so the tracking ref set below is exactly what was pushed.
	h, err := ex.Exec(ctx, "s-"+sb.ID, gitArgs("rev-parse", "--verify", "HEAD^{commit}"))
	head := strings.TrimSpace(h.Stdout)
	if err != nil || h.ExitCode != 0 || !gitimport.ValidCommitSHA(head) {
		return withAuto(syncFailed("sync_failed")), ""
	}
	var out gitimport.PushOutcome
	if err := s.withPaused(r, sb.ID, func() {
		out = runner.Push(ctx, gitimport.PushSpec{
			RepoURL: repoURL, Target: branch, Rev: head, Token: token, AppDir: appDir,
			ExpectHost: gitimport.HostOf(repoURL),
		})
	}); err != nil {
		return withAuto(syncFailed("sync_failed")), ""
	}
	if !out.OK {
		return withAuto(syncFailed(out.Reason)), ""
	}
	if u, err := ex.Exec(ctx, "s-"+sb.ID, syncGitArgs("update-ref", remoteRef, head)); err != nil || u.ExitCode != 0 {
		s.loggerFor(r, sb.ID).Warn("git sync: update tracking ref after publish failed", "branch", branch)
	}
	return withAuto(v1GitSyncResp{State: "published", Commits: commits}), ""
}

// POST /v1/apps/{id}/git/sync/abort
func (s *Server) v1GitSyncAbort(w http.ResponseWriter, r *http.Request) {
	app, err := s.Store.GetAppForOwner(r.Context(), r.PathValue("id"), tenantToken(r))
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such app")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "lookup failed")
		return
	}
	notAborted := func(reason string) {
		writeJSON(w, http.StatusOK, map[string]any{"aborted": false, "reason": reason})
	}
	sb, err := s.Store.CurrentSandboxForApp(r.Context(), app.ID)
	if err != nil {
		notAborted("no_workspace")
		return
	}
	if sb, err = s.wakeForGit(w, r, sb); err != nil {
		return
	}
	if sb.Status != "running" {
		notAborted("sandbox_not_running")
		return
	}

	s.Locks.Lock(gitLockKey(sb.ID))
	defer s.Locks.Unlock(gitLockKey(sb.ID))
	op := gitInProgress(filepath.Join(sb.WorkspaceMnt, "workspace", "app"))
	if op == "" {
		notAborted("nothing_in_progress")
		return
	}
	if !gitAbort(r.Context(), s.gitExec(), sb.ID, op) {
		notAborted("sync_failed")
		return
	}
	if err := s.Store.SetAppGitResolveTaskID(r.Context(), app.ID, ""); err != nil {
		s.loggerFor(r, sb.ID).Warn("git sync abort: clear resolve task failed", "err", err.Error())
	}
	s.auditAction(r, audit.Entry{Action: events.GitSyncAborted, Target: app.ID, Detail: map[string]any{"operation": op}})
	s.recordEvent(r, events.Event{Type: events.GitSyncAborted, Severity: events.SeverityInfo,
		Message: "Git " + op + " aborted", AppID: app.ID, SandboxID: sb.ID,
		Payload: map[string]any{"repo_url": app.GitRepoURL.String, "operation": op}})
	writeJSON(w, http.StatusOK, map[string]any{"aborted": true})
}

// errResponseWritten signals that wakeForGit already wrote the error response.
var errResponseWritten = errors.New("response written")

// wakeForGit wakes a stopped sandbox through the same internal path as task submit.
func (s *Server) wakeForGit(w http.ResponseWriter, r *http.Request, sb *store.Sandbox) (*store.Sandbox, error) {
	if sb.Status != "stopped" {
		return sb, nil
	}
	code, body := s.delegate(r, s.handleWakeJSON, http.MethodPost, "/wake/"+sb.ID,
		map[string]string{"id": sb.ID}, nil)
	if code != http.StatusOK {
		relayV1Error(w, code, body)
		return nil, errResponseWritten
	}
	fresh, err := s.Store.Get(r.Context(), sb.ID)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return nil, errResponseWritten
	}
	return fresh, nil
}

// submitResolveTask starts the conflict-resolution agent task via the public
// task-submit handler (default agent); "" = no agent could be started.
func (s *Server) submitResolveTask(r *http.Request, sandboxID, op, branch string) string {
	body, _ := json.Marshal(map[string]string{"prompt": resolvePrompt(op, branch)})
	code, out := s.delegate(r, s.v1SubmitTask, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/tasks",
		map[string]string{"id": sandboxID}, body)
	if code != http.StatusAccepted {
		return ""
	}
	var resp struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(out, &resp) != nil {
		return ""
	}
	return resp.ID
}

// resolvePrompt never embeds file names: they are repo-controlled (prompt injection).
func resolvePrompt(op, branch string) string {
	// The sandbox has no global git identity; pass the ephemeral one per command.
	next := "git -c user.name=sandbox-agent -c user.email=agent@sandboxd.local commit --no-edit"
	if op == "rebase" {
		next = "GIT_EDITOR=true git -c user.name=sandbox-agent -c user.email=agent@sandboxd.local rebase --continue"
	}
	p := "A git " + op + " of origin/" + branch + " into the local work stopped on conflicts. " +
		"List the conflicted files with `git diff --name-only --diff-filter=U`. " +
		"Resolve every conflict marker, preserving the intent of both sides. " +
		"Run the project's build/tests if available. `git add` the resolved files, then `" + next + "`."
	if op == "rebase" {
		p += " For a rebase, repeat until the rebase completes."
	}
	return p + " Do not push, do not reset, do not abort."
}

// syncGitArgs is gitArgs for in-sandbox merge/rebase/abort: no hooks, no
// signing, ephemeral identity (never written to .git/config).
func syncGitArgs(args ...string) []string {
	return gitArgs(append([]string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "commit.gpgsign=false",
		"-c", "user.name=sandbox-agent",
		"-c", "user.email=agent@sandboxd.local",
	}, args...)...)
}

func gitAbort(ctx context.Context, ex sandboxExecer, sbID, op string) bool {
	res, err := ex.Exec(ctx, "s-"+sbID, syncGitArgs(op, "--abort"))
	return err == nil && res.ExitCode == 0
}

// gitInProgress reports a stopped merge|rebase from the host workspace's .git
// markers ("" = none). A pure Lstat — no git runs host-side. A .git that is not
// a real directory reports none; the preflight refuses it.
func gitInProgress(appDir string) string {
	if !filepath.IsAbs(appDir) {
		return ""
	}
	if fi, err := os.Lstat(filepath.Join(appDir, ".git")); err != nil || !fi.IsDir() {
		return ""
	}
	exists := func(name string) bool {
		_, err := os.Lstat(filepath.Join(appDir, ".git", name))
		return err == nil
	}
	switch {
	case exists("rebase-merge"), exists("rebase-apply"):
		return "rebase"
	case exists("MERGE_HEAD"):
		return "merge"
	}
	return ""
}

// maxConflicts caps the repo-controlled conflict list returned to the console.
const maxConflicts = 200

// gitConflicts lists unmerged paths (in-sandbox, read-only), control chars stripped.
func gitConflicts(ctx context.Context, ex sandboxExecer, sbID string) []string {
	out := []string{}
	res, err := ex.Exec(ctx, "s-"+sbID, gitArgs("diff", "--name-only", "--diff-filter=U", "-z"))
	if err != nil || res.ExitCode != 0 {
		return out
	}
	for _, p := range strings.Split(res.Stdout, "\x00") {
		p = strings.Map(func(c rune) rune {
			if unicode.IsControl(c) {
				return -1
			}
			return c
		}, p)
		if p != "" && len(out) < maxConflicts {
			out = append(out, p)
		}
	}
	return out
}

func atoiTrim(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
