# Git Publish & Rebase-on-main — design

Status: approved design, pre-implementation. Date: 2026-10-20.

## Goal

Two new buttons in the console Git panel:

- **Publish** — commit any pending user changes, sync with the latest remote
  `main` (merge), and push the result to `main`.
- **Rebase on main** — commit any pending user changes and rebase the local work
  onto the latest remote `main`. Local only; nothing is pushed.

On conflict, an agent task resolves it automatically. An agent-resolved result is
**never pushed automatically** — the user reviews it and clicks Publish again.

## Decisions (from brainstorming)

| Question | Decision |
|---|---|
| Publish target | Push directly to the literal branch `main`, fast-forward only, never `--force` |
| Conflicts | Hand off to an agent task automatically |
| After agent resolution | Stop for user review; a second Publish click pushes |
| Uncommitted changes | Auto-commit all user files (runtime files excluded) first — both buttons |
| Rebase conflicts | Agent resolves each stop until the rebase completes; result stays local |
| Orchestration | Approach A: git itself is the state; one server endpoint; server starts the agent |

## Policy change vs. current `docs/git-workflow.md`

The current doc lists `fetch`/`pull`/`merge`, direct `main` push and shallow
deepening as explicit non-goals. This design deliberately reverses that **for the
sync endpoint only**:

- `POST git/push` (existing Push button) is unchanged — still new-branch only,
  `main`/`master`/import branch still rejected via `reservedDefaultBranches`.
- `POST git/sync` with `mode:"publish"` may write `refs/heads/main`, fast-forward
  only.

## API

### `POST /v1/apps/{id}/git/sync`

Body: `{"mode":"publish"|"rebase"}`. Any other mode → `400 invalid_request`.

Response (always `200` for domain outcomes, matching `git/push` style):

```json
{
  "state": "published|rebased|up_to_date|resolving|failed",
  "reason": "…",          // set when state=failed
  "commits": 3,            // published/rebased
  "auto_committed": "abc1234", // short sha if step 2 committed anything
  "task_id": "…",          // state=resolving
  "conflicts": ["src/a.ts"],   // state=resolving
  "operation": "merge|rebase"  // state=resolving
}
```

### `POST /v1/apps/{id}/git/sync/abort`

Runs `git merge --abort` or `git rebase --abort` (whichever is in progress) in the
sandbox, under the git lock. Returns `{"aborted":true}` or
`{"aborted":false,"reason":"nothing_in_progress"}`.

### `GET /v1/apps/{id}/git/status` — new fields

- `in_progress`: `"merge" | "rebase" | ""` — derived from `.git/MERGE_HEAD` /
  `.git/rebase-merge` / `.git/rebase-apply`.
- `conflicts`: list of unmerged paths (`git diff --name-only --diff-filter=U`).
- `resolve_task_id`: the most recent sync-resolution task for this app, if any
  (lets the console re-attach after a reload).

### `GET /v1/apps/{id}/git/diff?range=origin/main..HEAD`

New `range` mode alongside the existing per-path mode: combined diff of what
Publish would push. Only the exact value `origin/main..HEAD` is accepted (no
arbitrary revision strings). Same truncation cap as the per-path diff.

## Sync flow (`git/sync`)

The sandbox is woken first if stopped (same delegate path as `v1SubmitTask`).
Steps 1–5 run under the existing per-workspace lock `git:<sandbox-id>`; the lock
is released before the agent task runs.

1. **Pre-flight** — identical guards to push: valid `app.git_repo_url`,
   `app.git_credential_id`, workspace present, repo with `HEAD`,
   `AuditConfig` clean. If a merge/rebase is already in progress → return
   `state:"resolving"` with the existing `resolve_task_id` (idempotent; never
   starts a second merge or a second agent).
2. **Auto-commit** — if `git/status` reports user files, call the existing
   `gitCommit` with all user paths (runtime files excluded) and message
   `sandboxd: auto-commit before publish` / `… before rebase`.
3. **Fetch (host-side)** — new `gitimport.Runner.Fetch`:
   `git fetch <app repo URL> +refs/heads/main:refs/remotes/origin/main`, plus
   `--unshallow` when `RepoState.IsShallow`. Same hardening as `Push`: tokenless
   URL on argv (never `origin`), host-checking askpass, `roEnv()`,
   `GIT_ALLOW_PROTOCOL=https`, `core.hooksPath=<empty temp dir>` (fetch fires
   `reference-transaction`). Bounded by a timeout. Missing remote `main` →
   `no_main_branch`. Updating `refs/remotes/origin/main` also moves the baseline
   used by `Unpushed` and the ahead/behind badges.
4. **Integrate (in-sandbox, via `gitExec`)** — with the existing hardened flags,
   `--no-verify` where applicable, empty `core.hooksPath`, ephemeral author via
   `-c user.name= -c user.email=`:
   - publish: `git merge --no-edit origin/main`
   - rebase: `git rebase origin/main`
   Merge/rebase MUST NOT run host-side: repo-controlled merge drivers and
   clean/smudge filters would execute on the control plane.
5. **Outcome**
   - Nothing new from `main` and (publish) `Unpushed == 0` → `up_to_date`.
   - Clean, rebase → `rebased` with the local commit count.
   - Clean, publish → host-side `Push` of `HEAD:refs/heads/main` (no force). OK →
     `published`. Remote rejection → `failed` / `non_fast_forward` ("main moved
     — click Publish again").
   - Conflict → leave the operation in progress, collect unmerged paths, submit
     the resolution task (below), return `resolving`.
6. **Guard on other mutations** — while `in_progress != ""`, `git/commit`,
   `git/push` and `git/sync` refuse with `merge_in_progress` (sync returns the
   resolving state per step 1).

### Resolution task

Submitted through the same internal path as `POST /v1/sandboxes/{id}/tasks`,
default agent (`s.DefaultAgent`, falling back to `opencode`). Canned prompt,
parameterized with operation and conflicting files:

> A git {merge|rebase} of origin/main into the local work stopped on conflicts in:
> {files}. Resolve every conflict marker, preserving the intent of both sides. Run
> the project's build/tests if available. `git add` the resolved files, then
> {`git commit --no-edit` | `GIT_EDITOR=true git rebase --continue`}. For a rebase,
> repeat until the rebase completes. Do not push, do not reset, do not abort.

The task id is recorded on the app (new nullable column `git_resolve_task_id`)
so `git/status` can return it. If no agent is runnable → abort the operation and
return `failed` / `agent_unavailable` with `conflicts`.

The task finishing leaves the merge committed locally and unpushed. **That is the
review gate** — no extra state; the user's next Publish re-fetches and, if `main`
hasn't moved, merges nothing and pushes.

## Console (`console/src/AppView.tsx` Git panel)

- Buttons next to `Commit` / `Push`: **Rebase on main** (secondary) and
  **Publish** (primary). Enabled with uncommitted changes (tooltip: "commits all
  changed files first"). Disabled with a reason when there's no credential or
  `in_progress` is set. Busy labels `Publishing…` / `Rebasing…`.
- Toasts: `published` → "Published N commits → main"; `rebased` → "Rebased onto
  main (N local commits)"; `up_to_date` → "Already up to date";
  `non_fast_forward` → "main moved — click Publish again"; `no_credential` keeps
  the Settings hint.
- **Resolving banner** (driven by `git/status.in_progress`, survives reload):
  "Agent resolving N conflicts (merge|rebase)…", conflict file list, link to the
  task logs (existing task view). On task failure/cancel: "Agent couldn't
  resolve" + **Abort** button (`git/sync/abort`).
- **Review banner**: after an agent-resolved merge (task done, `in_progress` empty,
  ahead of `origin/main`): "Conflicts resolved by agent — review, then Publish",
  with **Review changes** opening the diff pane in `range=origin/main..HEAD`
  mode using the existing `DiffView`.
- api.ts: `gitSync(appId, mode)`, `gitSyncAbort(appId)`, `gitRangeDiff(appId)`;
  `GitStatus` gains `in_progress`, `conflicts`, `resolve_task_id`.

## Events & audit

`git.sync.published`, `git.sync.rebased`, `git.sync.conflict` (with task id +
file count), `git.sync.failed`, `git.sync.aborted`. Payload: tokenless repo URL,
mode, reason. Never the token or file contents.

## Error reasons

`no_repo_url`, `no_credential`, `credential_not_found`, `no_workspace`,
`not_a_git_repo`, `empty_repo_unsupported`, `unsafe_repo_config`,
`merge_in_progress`, `fetch_failed`, `no_main_branch`, `auth_failed`,
`non_fast_forward`, `agent_unavailable`, `sync_failed`.

## Security notes

- Token decrypted only control-plane-side, used only by host-side fetch + push,
  both with the host-checking askpass and `AuditConfig` run first.
- Merge, rebase and resolution execute in the sandbox — the only place
  repo-controlled drivers/filters/hooks may run. Sandbox still has no network
  and no credential.
- Writes to `main` are fast-forward only; the remote enforces it (no `--force`).
- Accepted risk: auto-commit publishes whatever user files exist in the
  workspace; no secret scanning. Document in `docs/git-workflow.md`.

## Testing

- Go unit tests (fake `pushRunner` extended with `Fetch`, fake in-sandbox exec as
  in `v1_git_commit_test.go`): each outcome (published, rebased, up_to_date,
  resolving + task submitted, non_fast_forward, agent_unavailable → aborted),
  auto-commit invoked only when dirty, in-progress idempotency, `merge_in_progress`
  refusal on commit/push, existing Push still rejects `main`, abort endpoint.
- Real-git test in `gitimport`: `Fetch` against a local bare repo — shallow →
  unshallow, ref updated, hooks not executed.
- Console tests: button enable/disable, toast mapping, resolving and review
  banners from status fields.

## Docs

- `docs/git-workflow.md`: new "Publish & Rebase" section; remove fetch/merge/main
  push from non-goals and limitations; state the accepted risks.
- `docs/openapi.yaml`: `git/sync`, `git/sync/abort`, `git/diff?range`, new status
  fields.

## Out of scope

Configurable target branch (fixed `main`), PR creation, auto-push after agent
resolution, check-command gating, SSH/provider APIs, secret scanning.
