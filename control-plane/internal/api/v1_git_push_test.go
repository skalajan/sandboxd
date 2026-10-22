package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/gitimport"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

type fakePusher struct {
	audit         []string
	state         gitimport.RepoState
	count         int
	baselineKnown bool
	outcome       gitimport.PushOutcome
	gotSpec       gitimport.PushSpec
	pushed        bool
	fetchOutcome  gitimport.FetchOutcome
	gotFetch      gitimport.FetchSpec
	fetched       bool
	pauser        *fakePauser // when set, Fetch/Push are logged into its call order
}

// fakePauser records pause/unpause (and, via fakePusher, fetch/push) in order.
type fakePauser struct {
	mu       sync.Mutex
	calls    []string
	paused   bool
	pauseErr error
}

func (p *fakePauser) Pause(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "pause "+name)
	if p.pauseErr != nil {
		return p.pauseErr
	}
	p.paused = true
	return nil
}
func (p *fakePauser) Unpause(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "unpause "+name)
	p.paused = false
	return nil
}
func (p *fakePauser) log(op string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused {
		op += " (paused)"
	}
	p.calls = append(p.calls, op)
}
func (p *fakePauser) isPaused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

// answer serves the in-sandbox repo queries (state, unpushed count, config
// audit) from the fake's fields; ok=false for any other command.
func (f *fakePusher) answer(cmd []string) (res docker.ExecResult, ok bool) {
	j := strings.Join(cmd, " ")
	out := func(s string) (docker.ExecResult, bool) { return docker.ExecResult{Stdout: s + "\n"}, true }
	fail := func(cond bool, s string) (docker.ExecResult, bool) {
		if cond {
			return docker.ExecResult{ExitCode: 128}, true
		}
		return out(s)
	}
	switch {
	case strings.HasSuffix(j, " rev-parse --is-inside-work-tree"):
		return fail(!f.state.IsRepo, "true")
	case strings.HasSuffix(j, " rev-parse --verify HEAD"):
		return fail(!f.state.HasHEAD, fakeHeadSHA)
	case strings.HasSuffix(j, " rev-parse --abbrev-ref HEAD"):
		if f.state.Detached {
			return out("HEAD")
		}
		return out("main")
	case strings.HasSuffix(j, " rev-parse --short HEAD"):
		return out(f.state.HeadShort)
	case strings.HasSuffix(j, " rev-parse --is-shallow-repository"):
		return out(strconv.FormatBool(f.state.IsShallow))
	case strings.Contains(j, " rev-parse --verify refs/remotes/origin/"):
		return fail(!f.baselineKnown, fakeHeadSHA)
	case strings.HasSuffix(j, " rev-list --count refs/remotes/origin/main..HEAD"), strings.HasSuffix(j, " rev-list --count HEAD"):
		return out(strconv.Itoa(f.count))
	case strings.Contains(j, " config --local --no-includes --list -z"):
		var b strings.Builder
		b.WriteString("core.bare\nfalse\x00")
		for _, k := range f.audit {
			b.WriteString(k + "\ntrue\x00")
		}
		return docker.ExecResult{Stdout: b.String()}, true
	}
	return docker.ExecResult{}, false
}

func (f *fakePusher) Exec(_ context.Context, _ string, cmd []string) (docker.ExecResult, error) {
	f.pauser.log("exec")
	res, _ := f.answer(cmd)
	return res, nil
}

// gitWorkspace makes a sandbox mnt whose workspace/app/.git passes CheckLayout.
func gitWorkspace(t *testing.T) string {
	t.Helper()
	mnt := t.TempDir()
	gitDir := filepath.Join(mnt, "workspace", "app", ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("[core]\n\tbare = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return mnt
}
func (f *fakePusher) Push(_ context.Context, spec gitimport.PushSpec) gitimport.PushOutcome {
	f.pauser.log("push")
	f.gotSpec = spec
	f.pushed = true
	return f.outcome
}
func (f *fakePusher) Fetch(_ context.Context, spec gitimport.FetchSpec) gitimport.FetchOutcome {
	f.pauser.log("fetch")
	f.gotFetch = spec
	f.fetched = true
	return f.fetchOutcome
}

// pushServer builds a server with Secrets + a fake pusher, a git-imported app
// (with a real sealed credential), and a sandbox in the given status.
func pushServer(t *testing.T, f *fakePusher, sandboxStatus string, withGit bool) (*Server, string) {
	t.Helper()
	s := appsGitServer(t) // Store + Secrets
	s.GitPush = f
	s.GitExec = f
	if f.pauser == nil {
		f.pauser = &fakePauser{}
	}
	s.GitPause = f.pauser
	app := &store.App{ID: newULID(), OwnerToken: "tenantA", Name: "My App"}
	if withGit {
		cid := seedCred(t, s, "tenantA", "gh") // seals "ghp_pat_secret"
		app.GitRepoURL = nullStr("https://github.com/o/r")
		app.GitBranch = nullStr("main")
		app.GitCredentialID = nullStr(cid)
	}
	if err := s.Store.CreateApp(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if sandboxStatus != "" {
		sb := &store.Sandbox{ID: newULID(), Status: sandboxStatus, WorkspaceMnt: gitWorkspace(t),
			AppID: sql.NullString{String: app.ID, Valid: true}}
		if err := s.Store.Create(context.Background(), sb); err != nil {
			t.Fatal(err)
		}
	}
	return s, app.ID
}

func postPush(s *Server, appID, owner, body string) *httptest.ResponseRecorder {
	r := reqAs("POST", "/v1/apps/"+appID+"/git/push", body, owner)
	r.SetPathValue("id", appID)
	w := httptest.NewRecorder()
	s.v1GitPush(w, r)
	return w
}

func okState() gitimport.RepoState {
	return gitimport.RepoState{IsRepo: true, HasHEAD: true, HeadShort: "abc1234"}
}

func TestPushSuccess(t *testing.T) {
	f := &fakePusher{state: okState(), count: 2, outcome: gitimport.PushOutcome{OK: true}}
	s, appID := pushServer(t, f, "running", true)
	w := postPush(s, appID, "tenantA", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	var resp v1GitPushResp
	json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Pushed || resp.Commits != 2 || resp.RemoteURL != "https://github.com/o/r" {
		t.Fatalf("resp = %+v", resp)
	}
	if !strings.HasPrefix(resp.Branch, "sandboxd/my-app-") {
		t.Errorf("default branch = %q", resp.Branch)
	}
	// the credential was decrypted ONLY in the push path and reached the pusher
	if f.gotSpec.Token != "ghp_pat_secret" {
		t.Errorf("pusher did not receive the decrypted token")
	}
	if f.gotSpec.ExpectHost != "github.com" {
		t.Errorf("ExpectHost = %q", f.gotSpec.ExpectHost)
	}
	// the token must never appear in the response
	if strings.Contains(w.Body.String(), "ghp_pat_secret") {
		t.Fatal("token leaked into the push response")
	}
}

// Push needs a running sandbox (repo queries run in it); only "stopped" is woken.
func TestPushRequiresRunningSandbox(t *testing.T) {
	f := &fakePusher{state: okState(), count: 1, outcome: gitimport.PushOutcome{OK: true}}
	s, appID := pushServer(t, f, "creating", true)
	var resp v1GitPushResp
	json.Unmarshal(postPush(s, appID, "tenantA", `{}`).Body.Bytes(), &resp)
	if resp.Pushed || resp.Reason != "sandbox_not_running" || f.pushed {
		t.Errorf("resp = %+v pushed=%v", resp, f.pushed)
	}
}

// The host .git layout is checked in Go before any query or push.
func TestPushRefusesUnsafeGitLayout(t *testing.T) {
	f := &fakePusher{state: okState(), count: 1, outcome: gitimport.PushOutcome{OK: true}}
	s, appID := pushServer(t, f, "running", true)
	sb, _ := s.Store.CurrentSandboxForApp(context.Background(), appID)
	os.WriteFile(filepath.Join(sb.WorkspaceMnt, "workspace", "app", ".git", "config"), []byte("[include]\n\tpath = /tmp/x\n"), 0o644)
	var resp v1GitPushResp
	json.Unmarshal(postPush(s, appID, "tenantA", `{}`).Body.Bytes(), &resp)
	if resp.Reason != "unsafe_repo_config" || f.pushed {
		t.Errorf("resp = %+v pushed=%v", resp, f.pushed)
	}
}

func TestPushPreflightReasons(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(f *fakePusher)
		body   string
		git    bool
		status string
		want   string
	}{
		{"no_repo_url", func(f *fakePusher) { f.state = okState() }, `{}`, false, "running", "no_repo_url"},
		{"not_a_git_repo", func(f *fakePusher) { f.state = gitimport.RepoState{IsRepo: false} }, `{}`, true, "running", "not_a_git_repo"},
		{"empty_repo", func(f *fakePusher) { f.state = gitimport.RepoState{IsRepo: true, HasHEAD: false} }, `{}`, true, "running", "empty_repo_unsupported"},
		{"unsafe_config", func(f *fakePusher) { f.state = okState(); f.audit = []string{"url.x.insteadof"} }, `{}`, true, "running", "unsafe_repo_config"},
		{"no_local_commits", func(f *fakePusher) { f.state = okState(); f.count = 0 }, `{}`, true, "running", "no_local_commits"},
		{"refuses_main", func(f *fakePusher) { f.state = okState(); f.count = 1 }, `{"branch":"main"}`, true, "running", "refuses_default_branch"},
		{"refuses_import_branch", func(f *fakePusher) { f.state = okState(); f.count = 1 }, `{"branch":"main"}`, true, "running", "refuses_default_branch"},
		{"no_workspace", func(f *fakePusher) { f.state = okState() }, `{}`, true, "", "no_workspace"},
		{"push_branch_exists", func(f *fakePusher) {
			f.state = okState()
			f.count = 1
			f.outcome = gitimport.PushOutcome{Reason: "branch_exists"}
		}, `{}`, true, "running", "branch_exists"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakePusher{}
			c.setup(f)
			s, appID := pushServer(t, f, c.status, c.git)
			w := postPush(s, appID, "tenantA", c.body)
			var resp v1GitPushResp
			json.Unmarshal(w.Body.Bytes(), &resp)
			if resp.Pushed || resp.Reason != c.want {
				t.Errorf("got pushed=%v reason=%q want %q (%s)", resp.Pushed, resp.Reason, c.want, w.Body.String())
			}
		})
	}
}

func TestPushCredentialAndAuthGuards(t *testing.T) {
	// credential id set but the row doesn't exist -> credential_not_found
	f := &fakePusher{state: okState(), count: 1}
	s := appsGitServer(t)
	s.GitPush, s.GitExec, s.GitPause = f, f, &fakePauser{}
	app := &store.App{ID: newULID(), OwnerToken: "tenantA", Name: "imp",
		GitRepoURL: nullStr("https://github.com/o/r"), GitBranch: nullStr("main"),
		GitCredentialID: nullStr("01MISSINGCRED")}
	s.Store.CreateApp(context.Background(), app)
	sb := &store.Sandbox{ID: newULID(), Status: "running", WorkspaceMnt: gitWorkspace(t),
		AppID: sql.NullString{String: app.ID, Valid: true}}
	s.Store.Create(context.Background(), sb)
	w := postPush(s, app.ID, "tenantA", `{}`)
	if !strings.Contains(w.Body.String(), "credential_not_found") {
		t.Errorf("expected credential_not_found: %s", w.Body.String())
	}
	// the fake pusher must NOT have been called (we never reached the push)
	if f.pushed {
		t.Error("push ran despite missing credential")
	}
}

func TestPushOwnerAndValidation(t *testing.T) {
	f := &fakePusher{state: okState(), count: 1, outcome: gitimport.PushOutcome{OK: true}}
	s, appID := pushServer(t, f, "running", true)

	// cross-owner -> 404
	if w := postPush(s, appID, "tenantB", `{}`); w.Code != http.StatusNotFound {
		t.Errorf("cross-owner: got %d", w.Code)
	}
	// invalid branch ref -> 400
	if w := postPush(s, appID, "tenantA", `{"branch":"../evil"}`); w.Code != http.StatusBadRequest {
		t.Errorf("invalid branch: got %d", w.Code)
	}
	// no credential associated -> no_credential
	app2 := &store.App{ID: newULID(), OwnerToken: "tenantA", Name: "x", GitRepoURL: nullStr("https://github.com/o/r")}
	s.Store.CreateApp(context.Background(), app2)
	sb := &store.Sandbox{ID: newULID(), Status: "running", WorkspaceMnt: t.TempDir(),
		AppID: sql.NullString{String: app2.ID, Valid: true}}
	s.Store.Create(context.Background(), sb)
	if w := postPush(s, app2.ID, "tenantA", `{}`); !strings.Contains(w.Body.String(), "no_credential") {
		t.Errorf("expected no_credential: %s", w.Body.String())
	}
}

// The repo queries run in the sandbox first; the host-side push runs only while it is frozen.
func TestPushPausesSandbox(t *testing.T) {
	f := &fakePusher{state: okState(), count: 1, outcome: gitimport.PushOutcome{OK: true}}
	s, appID := pushServer(t, f, "running", true)
	sb, _ := s.Store.CurrentSandboxForApp(context.Background(), appID)
	postPush(s, appID, "tenantA", `{}`)
	calls := strings.Join(f.pauser.calls, "|")
	if !strings.HasSuffix(calls, "exec|pause s-"+sb.ID+"|push (paused)|unpause s-"+sb.ID) || strings.Contains(calls, "exec (paused)") {
		t.Errorf("calls = %s", calls)
	}
}

func TestPushRefusedWhenPauseFails(t *testing.T) {
	f := &fakePusher{state: okState(), count: 1, outcome: gitimport.PushOutcome{OK: true},
		pauser: &fakePauser{pauseErr: errors.New("boom")}}
	s, appID := pushServer(t, f, "running", true)
	var resp v1GitPushResp
	json.Unmarshal(postPush(s, appID, "tenantA", `{}`).Body.Bytes(), &resp)
	if resp.Pushed || resp.Reason != "push_failed" || f.pushed {
		t.Errorf("resp = %+v pushed=%v", resp, f.pushed)
	}
}
