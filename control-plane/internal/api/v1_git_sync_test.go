package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/gitimport"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/loopback"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// syncFake is the in-sandbox git for sync: a failed merge/rebase drops the
// matching .git marker (left in progress), --abort removes it.
type syncFake struct {
	mu              sync.Mutex
	gitDir          string
	statusOut       string
	behind          string
	integrateExit   int
	conflicts       string
	cmds            [][]string
	pauser          *fakePauser
	repo            *fakePusher // answers the in-sandbox repo queries
	execWhilePaused bool
}

const fakeHeadSHA = "abcdef1234567890abcdef1234567890abcdef12"

func (f *syncFake) Exec(_ context.Context, _ string, cmd []string) (docker.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pauser != nil && f.pauser.isPaused() {
		f.execWhilePaused = true
	}
	f.cmds = append(f.cmds, cmd)
	if f.repo != nil {
		if res, ok := f.repo.answer(cmd); ok {
			return res, nil
		}
	}
	j := strings.Join(cmd, " ")
	switch {
	case strings.Contains(j, " status "):
		return docker.ExecResult{Stdout: f.statusOut}, nil
	case strings.Contains(j, "--abort"):
		os.RemoveAll(filepath.Join(f.gitDir, "MERGE_HEAD"))
		os.RemoveAll(filepath.Join(f.gitDir, "rebase-merge"))
		return docker.ExecResult{}, nil
	case strings.Contains(j, "rev-list --count"):
		return docker.ExecResult{Stdout: f.behind + "\n"}, nil
	case strings.Contains(j, "--diff-filter=U"):
		return docker.ExecResult{Stdout: f.conflicts}, nil
	case strings.Contains(j, " merge "):
		if f.integrateExit != 0 {
			os.WriteFile(filepath.Join(f.gitDir, "MERGE_HEAD"), []byte("x\n"), 0o644)
		}
		return docker.ExecResult{ExitCode: f.integrateExit}, nil
	case strings.Contains(j, " rebase "):
		if f.integrateExit != 0 {
			os.MkdirAll(filepath.Join(f.gitDir, "rebase-merge"), 0o755)
		}
		return docker.ExecResult{ExitCode: f.integrateExit}, nil
	case strings.Contains(j, "rev-parse"):
		return docker.ExecResult{Stdout: fakeHeadSHA + "\n"}, nil
	}
	return docker.ExecResult{}, nil
}

func (f *syncFake) cmdContaining(sub string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if strings.Contains(strings.Join(c, " "), sub) {
			return c
		}
	}
	return nil
}
func (f *syncFake) issued(sub string) bool { return f.cmdContaining(sub) != nil }

// fakeTaskRuntimed accepts task submissions on <root>/<sbID>/.runtimed/sock and
// records each submitted prompt.
type fakeTaskRuntimed struct {
	mu       sync.Mutex
	prompts  []string
	onSubmit func()
}

func (rt *fakeTaskRuntimed) last() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.prompts) == 0 {
		return ""
	}
	return rt.prompts[len(rt.prompts)-1]
}

func startTaskRuntimed(t *testing.T, root, sbID string) *fakeTaskRuntimed {
	t.Helper()
	dir := filepath.Join(root, sbID, ".runtimed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	rt := &fakeTaskRuntimed{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt string `json:"prompt"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		rt.mu.Lock()
		rt.prompts = append(rt.prompts, body.Prompt)
		hook := rt.onSubmit
		rt.mu.Unlock()
		if hook != nil {
			hook()
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /tasks/{id}/events", func(w http.ResponseWriter, r *http.Request) {})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return rt
}

type syncEnv struct {
	s     *Server
	appID string
	sbID  string
	git   *syncFake
	push  *fakePusher
	rt    *fakeTaskRuntimed
}

// syncServer wires a git-imported app with a running sandbox, fake host-side
// git, fake in-sandbox git and a fake runtimed (short root: unix socket paths
// are length-limited).
func syncServer(t *testing.T, push *fakePusher, git *syncFake) *syncEnv {
	t.Helper()
	s := appsGitServer(t)
	s.Store = fileStore(t) // the background task watcher needs a shared (non-:memory:) DB
	s.GitPush = push
	s.GitExec = git
	if push.pauser == nil {
		push.pauser = &fakePauser{}
	}
	git.pauser, git.repo = push.pauser, push
	s.GitPause = push.pauser
	s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	root, err := os.MkdirTemp("", "rt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	s.Loopback = &loopback.Manager{Root: root}

	cid := seedCred(t, s, "tenantA", "gh")
	app := &store.App{ID: newULID(), OwnerToken: "tenantA", Name: "My App",
		GitRepoURL: nullStr("https://github.com/o/r"), GitBranch: nullStr("main"), GitCredentialID: nullStr(cid)}
	if err := s.Store.CreateApp(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	const sbID = "sbsync"
	mnt := t.TempDir()
	git.gitDir = filepath.Join(mnt, "workspace", "app", ".git")
	if err := os.MkdirAll(git.gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(git.gitDir, "config"), []byte("[core]\n\tbare = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb := &store.Sandbox{ID: sbID, Status: "running", WorkspaceMnt: mnt,
		AppID: sql.NullString{String: app.ID, Valid: true}}
	if err := s.Store.Create(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	return &syncEnv{s: s, appID: app.ID, sbID: sbID, git: git, push: push, rt: startTaskRuntimed(t, root, sbID)}
}

func (e *syncEnv) sync(t *testing.T, owner, body string) (int, v1GitSyncResp) {
	t.Helper()
	r := reqAs("POST", "/v1/apps/"+e.appID+"/git/sync", body, owner)
	r.SetPathValue("id", e.appID)
	w := httptest.NewRecorder()
	e.s.v1GitSync(w, r)
	var resp v1GitSyncResp
	json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp
}

func (e *syncEnv) abort(t *testing.T) map[string]any {
	t.Helper()
	r := reqAs("POST", "/v1/apps/"+e.appID+"/git/sync/abort", "", "tenantA")
	r.SetPathValue("id", e.appID)
	w := httptest.NewRecorder()
	e.s.v1GitSyncAbort(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

const cleanStatus = "## main\x00"

func TestSyncPublishMergesAndPushesMain(t *testing.T) {
	push := &fakePusher{state: okState(), count: 3, outcome: gitimport.PushOutcome{OK: true},
		fetchOutcome: gitimport.FetchOutcome{OK: true}}
	e := syncServer(t, push, &syncFake{statusOut: dirtyStatus, behind: "2"})
	code, resp := e.sync(t, "tenantA", `{"mode":"publish"}`)
	if code != http.StatusOK || resp.State != "published" || resp.Commits != 3 {
		t.Fatalf("got %d %+v", code, resp)
	}
	if resp.AutoCommitted != "abcdef1" {
		t.Errorf("auto_committed = %q", resp.AutoCommitted)
	}
	commit := strings.Join(e.git.cmdContaining(" commit "), " ")
	if !strings.Contains(commit, "sandboxd: auto-commit before publish") || strings.Contains(commit, "pnpm-lock.yaml") {
		t.Errorf("auto-commit argv = %s", commit)
	}
	merge := strings.Join(e.git.cmdContaining(" merge "), " ")
	for _, want := range []string{"--no-edit", "--no-verify", "core.hooksPath=/dev/null", "user.name=", "user.email=", "refs/remotes/origin/main"} {
		if !strings.Contains(merge, want) {
			t.Errorf("merge argv missing %q: %s", want, merge)
		}
	}
	if !push.fetched || push.gotFetch.Branch != "main" || push.gotFetch.Token != "ghp_pat_secret" || push.gotFetch.Unshallow {
		t.Errorf("fetch spec = %+v", push.gotFetch)
	}
	if !push.pushed || push.gotSpec.Target != "main" || push.gotSpec.Rev != fakeHeadSHA {
		t.Errorf("push spec = %+v", push.gotSpec)
	}
	// the sandbox is frozen exactly around fetch and push, never across an exec
	want := "pause s-sbsync|fetch (paused)|unpause s-sbsync|pause s-sbsync|push (paused)|unpause s-sbsync"
	if got := strings.Join(push.pauser.calls, "|"); got != want {
		t.Errorf("pause order = %s", got)
	}
	if e.git.execWhilePaused {
		t.Error("in-sandbox exec ran while the sandbox was paused")
	}
	// the local tracking ref moves to exactly the pushed commit
	upd := strings.Join(e.git.cmdContaining("update-ref"), " ")
	if !strings.HasSuffix(upd, "update-ref refs/remotes/origin/main "+fakeHeadSHA) || !strings.Contains(upd, "core.hooksPath=/dev/null") {
		t.Errorf("update-ref argv = %q", upd)
	}
}

func TestSyncPauseFailureRefusesFetch(t *testing.T) {
	push := &fakePusher{state: okState(), fetchOutcome: gitimport.FetchOutcome{OK: true},
		pauser: &fakePauser{pauseErr: errors.New("boom")}}
	e := syncServer(t, push, &syncFake{statusOut: cleanStatus})
	if _, resp := e.sync(t, "tenantA", `{"mode":"publish"}`); resp.State != "failed" || push.fetched {
		t.Errorf("resp = %+v fetched=%v", resp, push.fetched)
	}
}

func TestSyncFailedPushDoesNotMoveTrackingRef(t *testing.T) {
	push := &fakePusher{state: okState(), count: 1, fetchOutcome: gitimport.FetchOutcome{OK: true},
		outcome: gitimport.PushOutcome{Reason: "non_fast_forward"}}
	e := syncServer(t, push, &syncFake{statusOut: cleanStatus, behind: "0"})
	e.sync(t, "tenantA", `{"mode":"publish"}`)
	if e.git.issued("update-ref") {
		t.Error("update-ref must only follow a successful publish")
	}
}

func TestSyncRebaseCleanIsLocalOnly(t *testing.T) {
	push := &fakePusher{state: okState(), count: 2, fetchOutcome: gitimport.FetchOutcome{OK: true}}
	e := syncServer(t, push, &syncFake{statusOut: cleanStatus, behind: "1"})
	_, resp := e.sync(t, "tenantA", `{"mode":"rebase"}`)
	if resp.State != "rebased" || resp.Commits != 2 || resp.AutoCommitted != "" {
		t.Fatalf("resp = %+v", resp)
	}
	if e.git.issued(" commit ") {
		t.Error("auto-commit must not run on a clean workspace")
	}
	rebase := strings.Join(e.git.cmdContaining(" rebase "), " ")
	if !strings.Contains(rebase, "--no-verify") || !strings.Contains(rebase, "refs/remotes/origin/main") {
		t.Errorf("rebase argv = %s", rebase)
	}
	if push.pushed {
		t.Error("rebase must never push")
	}
}

func TestSyncUpToDate(t *testing.T) {
	for _, mode := range []string{"publish", "rebase"} {
		t.Run(mode, func(t *testing.T) {
			push := &fakePusher{state: okState(), count: 0, fetchOutcome: gitimport.FetchOutcome{OK: true}}
			e := syncServer(t, push, &syncFake{statusOut: cleanStatus, behind: "0"})
			_, resp := e.sync(t, "tenantA", `{"mode":"`+mode+`"}`)
			if resp.State != "up_to_date" {
				t.Fatalf("resp = %+v", resp)
			}
			if e.git.issued(" merge ") || e.git.issued(" rebase ") || push.pushed {
				t.Error("nothing to integrate or push when up to date")
			}
		})
	}
}

func TestSyncPublishNonFastForward(t *testing.T) {
	push := &fakePusher{state: okState(), count: 1, fetchOutcome: gitimport.FetchOutcome{OK: true},
		outcome: gitimport.PushOutcome{Reason: "non_fast_forward"}}
	e := syncServer(t, push, &syncFake{statusOut: cleanStatus, behind: "0"})
	_, resp := e.sync(t, "tenantA", `{"mode":"publish"}`)
	if resp.State != "failed" || resp.Reason != "non_fast_forward" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestSyncConflictSubmitsResolveTask(t *testing.T) {
	push := &fakePusher{state: okState(), count: 1, outcome: gitimport.PushOutcome{OK: true},
		fetchOutcome: gitimport.FetchOutcome{OK: true}}
	e := syncServer(t, push, &syncFake{statusOut: cleanStatus, behind: "1", integrateExit: 1, conflicts: "src/a.ts\x00src/b.ts\x00"})
	_, resp := e.sync(t, "tenantA", `{"mode":"publish"}`)
	if resp.State != "resolving" || resp.Operation != "merge" || resp.TaskID == "" || len(resp.Conflicts) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	if push.pushed {
		t.Fatal("a conflicted merge must not be pushed")
	}
	p := e.rt.last()
	if !strings.Contains(p, "git merge") || !strings.Contains(p, "`git diff --name-only --diff-filter=U`") || !strings.Contains(p, "`git -c user.name=sandbox-agent -c user.email=agent@sandboxd.local commit --no-edit`") {
		t.Errorf("prompt = %q", p)
	}
	if strings.Contains(p, "src/a.ts") || strings.Contains(p, "src/b.ts") {
		t.Errorf("prompt must not embed repo-controlled file names: %q", p)
	}
	if id, _ := e.s.Store.GetAppGitResolveTaskID(context.Background(), e.appID); id != resp.TaskID {
		t.Errorf("stored resolve task = %q, want %q", id, resp.TaskID)
	}

	// status reports the in-progress operation for the console to re-attach
	r := reqAs("GET", "/v1/apps/"+e.appID+"/git/status", "", "tenantA")
	r.SetPathValue("id", e.appID)
	w := httptest.NewRecorder()
	e.s.v1GitStatus(w, r)
	var st v1GitStatusResp
	json.Unmarshal(w.Body.Bytes(), &st)
	if st.InProgress != "merge" || len(st.Conflicts) != 2 || st.ResolveTaskID != resp.TaskID {
		t.Errorf("status = %+v", st)
	}

	// idempotent: a second sync neither merges again nor starts a second agent
	e.git.cmds, push.fetched = nil, false
	_, again := e.sync(t, "tenantA", `{"mode":"publish"}`)
	if again.State != "resolving" || again.TaskID != resp.TaskID || again.Operation != "merge" {
		t.Errorf("second sync = %+v", again)
	}
	if push.fetched || e.git.issued(" merge ") || len(e.rt.prompts) != 1 {
		t.Error("in-progress sync must not fetch, merge or submit again")
	}

	// commit and push refuse while the merge is in progress
	if w := postCommit(e.s, e.appID, "tenantA", `{"message":"m"}`); !strings.Contains(w.Body.String(), "merge_in_progress") {
		t.Errorf("commit: %s", w.Body.String())
	}
	if w := postPush(e.s, e.appID, "tenantA", `{}`); !strings.Contains(w.Body.String(), "merge_in_progress") {
		t.Errorf("push: %s", w.Body.String())
	}
}

func TestSyncRebaseConflictPrompt(t *testing.T) {
	push := &fakePusher{state: okState(), fetchOutcome: gitimport.FetchOutcome{OK: true}}
	e := syncServer(t, push, &syncFake{statusOut: cleanStatus, behind: "1", integrateExit: 1, conflicts: "a.ts\x00"})
	_, resp := e.sync(t, "tenantA", `{"mode":"rebase"}`)
	if resp.State != "resolving" || resp.Operation != "rebase" {
		t.Fatalf("resp = %+v", resp)
	}
	if p := e.rt.last(); !strings.Contains(p, "`GIT_EDITOR=true git -c user.name=sandbox-agent -c user.email=agent@sandboxd.local rebase --continue`") || !strings.Contains(p, "repeat until the rebase completes") {
		t.Errorf("prompt = %q", p)
	}
}

func TestSyncAgentUnavailableAborts(t *testing.T) {
	push := &fakePusher{state: okState(), fetchOutcome: gitimport.FetchOutcome{OK: true}}
	e := syncServer(t, push, &syncFake{statusOut: cleanStatus, behind: "1", integrateExit: 1, conflicts: "a.ts\x00"})
	e.s.DefaultAgent = "minimax" // not runnable as a task agent
	_, resp := e.sync(t, "tenantA", `{"mode":"publish"}`)
	if resp.State != "failed" || resp.Reason != "agent_unavailable" || len(resp.Conflicts) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	if !e.git.issued("merge --abort") {
		t.Error("the merge must be aborted when no agent can resolve it")
	}
	if gitInProgress(filepath.Dir(e.git.gitDir)) != "" {
		t.Error("merge still in progress after abort")
	}
}

func TestSyncFetchAndShallow(t *testing.T) {
	st := okState()
	st.IsShallow = true
	push := &fakePusher{state: st, fetchOutcome: gitimport.FetchOutcome{Reason: "no_main_branch"}}
	e := syncServer(t, push, &syncFake{statusOut: cleanStatus})
	_, resp := e.sync(t, "tenantA", `{"mode":"publish"}`)
	if resp.State != "failed" || resp.Reason != "no_main_branch" {
		t.Fatalf("resp = %+v", resp)
	}
	if !push.gotFetch.Unshallow {
		t.Error("a shallow repo must be fetched with --unshallow")
	}
}

func TestSyncValidationAndPreflight(t *testing.T) {
	push := &fakePusher{state: okState(), fetchOutcome: gitimport.FetchOutcome{OK: true}}
	e := syncServer(t, push, &syncFake{statusOut: cleanStatus})
	if code, _ := e.sync(t, "tenantA", `{"mode":"force"}`); code != http.StatusBadRequest {
		t.Errorf("bad mode: %d", code)
	}
	if code, _ := e.sync(t, "tenantA", ``); code != http.StatusBadRequest {
		t.Errorf("empty body: %d", code)
	}
	if code, _ := e.sync(t, "tenantB", `{"mode":"publish"}`); code != http.StatusNotFound {
		t.Errorf("cross-owner: %d", code)
	}
	push.audit = []string{"core.hookspath"}
	if _, resp := e.sync(t, "tenantA", `{"mode":"publish"}`); resp.Reason != "unsafe_repo_config" || push.fetched {
		t.Errorf("unsafe config: %+v fetched=%v", resp, push.fetched)
	}

	app := &store.App{ID: newULID(), OwnerToken: "tenantA", Name: "x", GitRepoURL: nullStr("https://github.com/o/r")}
	e.s.Store.CreateApp(context.Background(), app)
	r := reqAs("POST", "/v1/apps/"+app.ID+"/git/sync", `{"mode":"publish"}`, "tenantA")
	r.SetPathValue("id", app.ID)
	w := httptest.NewRecorder()
	e.s.v1GitSync(w, r)
	if !strings.Contains(w.Body.String(), "no_credential") {
		t.Errorf("expected no_credential: %s", w.Body.String())
	}
}

func TestSyncAbort(t *testing.T) {
	e := syncServer(t, &fakePusher{state: okState()}, &syncFake{statusOut: cleanStatus})
	if out := e.abort(t); out["aborted"] != false || out["reason"] != "nothing_in_progress" {
		t.Fatalf("idle abort = %v", out)
	}
	os.MkdirAll(filepath.Join(e.git.gitDir, "rebase-merge"), 0o755)
	e.s.Store.SetAppGitResolveTaskID(context.Background(), e.appID, "T1")
	if out := e.abort(t); out["aborted"] != true {
		t.Fatalf("abort = %v", out)
	}
	if !e.git.issued("rebase --abort") {
		t.Error("expected git rebase --abort")
	}
	if id, _ := e.s.Store.GetAppGitResolveTaskID(context.Background(), e.appID); id != "" {
		t.Errorf("resolve task not cleared: %q", id)
	}
}

func TestGitDiffRange(t *testing.T) {
	ex := &fakeExecer{}
	s, appID := commitServer(t, true)
	s.GitExec = ex
	get := func(q string) *httptest.ResponseRecorder {
		r := reqAs("GET", "/v1/apps/"+appID+"/git/diff?"+q, "", "tenantA")
		r.SetPathValue("id", appID)
		w := httptest.NewRecorder()
		s.v1GitDiff(w, r)
		return w
	}
	if w := get("range=HEAD~3..HEAD"); w.Code != http.StatusBadRequest {
		t.Errorf("arbitrary range: %d", w.Code)
	}
	if w := get("range=origin/main..HEAD"); w.Code != http.StatusOK {
		t.Fatalf("range: %d %s", w.Code, w.Body.String())
	}
	last := strings.Join(ex.gotCmd[len(ex.gotCmd)-1], " ")
	if !strings.Contains(last, "diff --no-ext-diff --no-textconv refs/remotes/origin/main...HEAD") {
		t.Errorf("range diff argv = %s", last)
	}
}

// A finished sync clears the stale resolve task id (the console's review banner
// keys on it); a failed sync keeps it.
func TestSyncClearsResolveTaskOnSuccess(t *testing.T) {
	cases := []struct {
		name, mode, behind, want string
		count                    int
		push                     gitimport.PushOutcome
		keep                     bool
	}{
		{"published", "publish", "0", "published", 1, gitimport.PushOutcome{OK: true}, false},
		{"rebased", "rebase", "1", "rebased", 1, gitimport.PushOutcome{}, false},
		{"up_to_date", "publish", "0", "up_to_date", 0, gitimport.PushOutcome{}, false},
		{"failed_keeps", "publish", "0", "failed", 1, gitimport.PushOutcome{Reason: "non_fast_forward"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			push := &fakePusher{state: okState(), count: c.count, outcome: c.push,
				fetchOutcome: gitimport.FetchOutcome{OK: true}}
			e := syncServer(t, push, &syncFake{statusOut: cleanStatus, behind: c.behind})
			e.s.Store.SetAppGitResolveTaskID(context.Background(), e.appID, "OLD")
			if _, resp := e.sync(t, "tenantA", `{"mode":"`+c.mode+`"}`); resp.State != c.want {
				t.Fatalf("resp = %+v", resp)
			}
			id, _ := e.s.Store.GetAppGitResolveTaskID(context.Background(), e.appID)
			if c.keep && id != "OLD" || !c.keep && id != "" {
				t.Errorf("resolve task id = %q (keep=%v)", id, c.keep)
			}
		})
	}
}

// The resolve task is submitted with the git lock released.
func TestSyncReleasesLockBeforeSubmit(t *testing.T) {
	push := &fakePusher{state: okState(), fetchOutcome: gitimport.FetchOutcome{OK: true}}
	e := syncServer(t, push, &syncFake{statusOut: cleanStatus, behind: "1", integrateExit: 1, conflicts: "a.ts\x00"})
	var free atomic.Bool
	e.rt.mu.Lock()
	e.rt.onSubmit = func() {
		if e.s.Locks.TryLock(gitLockKey(e.sbID)) {
			free.Store(true)
			e.s.Locks.Unlock(gitLockKey(e.sbID))
		}
	}
	e.rt.mu.Unlock()
	if _, resp := e.sync(t, "tenantA", `{"mode":"publish"}`); resp.State != "resolving" || resp.TaskID == "" {
		t.Fatalf("resp = %+v", resp)
	}
	if !free.Load() {
		t.Error("git lock was held while submitting the resolve task")
	}
	if !e.s.Locks.TryLock(gitLockKey(e.sbID)) {
		t.Error("git lock leaked after sync")
	}
}

// Conflict names are repo-controlled: control chars stripped, list capped.
func TestGitConflictsSanitized(t *testing.T) {
	var b strings.Builder
	b.WriteString("evil\x1b[31m\nname.ts\x00")
	for i := 0; i < maxConflicts+50; i++ {
		b.WriteString("f" + strconv.Itoa(i) + "\x00")
	}
	got := gitConflicts(context.Background(), &syncFake{conflicts: b.String()}, "x")
	if len(got) != maxConflicts || got[0] != "evil[31mname.ts" {
		t.Errorf("len=%d first=%q", len(got), got[0])
	}
}

func TestGitInProgressIgnoresNonDirGitDir(t *testing.T) {
	app := t.TempDir()
	target := filepath.Join(t.TempDir(), ".git")
	os.MkdirAll(filepath.Join(target, "rebase-merge"), 0o755)
	os.Symlink(target, filepath.Join(app, ".git"))
	if op := gitInProgress(app); op != "" {
		t.Errorf("symlinked .git: %q", op)
	}
	app2 := t.TempDir()
	os.WriteFile(filepath.Join(app2, ".git"), []byte("gitdir: "+target+"\n"), 0o644)
	if op := gitInProgress(app2); op != "" {
		t.Errorf("gitdir file: %q", op)
	}
}
