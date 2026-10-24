# Git workflow (optional)

Git support is an **optional workflow layered on top of sandboxd core** — a way to
import a repo into an app, inspect what the agent changed, and commit/push it back.
It is driven entirely through the public `/v1` API; the web console adds a UI for
it, but the console is optional and the core engine works without any of this. If
you never touch the Git endpoints, nothing here is active.

It is intentionally small and HTTPS-based (no SSH, no GitHub/GitLab App, no
provider APIs). Two import modes:

- **Public repos** — a curated **starter project** or any public URL clones
  **tokenless**: pass `git:{repo_url, branch}` with **no** `credential_id`. The
  URL is tokenless and `GIT_TERMINAL_PROMPT=0` makes a private repo fail cleanly
  rather than hang.
- **Private repos** — reference an encrypted, owner-scoped **PAT** by
  `credential_id`; it is decrypted host-side at clone time via a `GIT_ASKPASS`
  helper and never enters the sandbox.

For claude-code subscriptions there is also a real server-side OAuth flow (see
[`agent-auth.md`](./agent-auth.md)) — that is agent auth, separate from Git PATs.

---

## The end-to-end flow

1. **Add a credential** (once): store an encrypted, owner-scoped PAT.
2. **Import**: create an app from a private repo URL using that credential.
   sandboxd clones it host-side into the workspace before the sandbox starts.
3. **Inspect the runtime** (advisory): detect the framework + suggested preset.
4. **Work**: the agent edits files in the sandbox as usual.
5. **Review**: read-only `git status` / `git diff` of the workspace.
6. **Commit**: commit selected changes locally (in-sandbox).
7. **Push**: push the local commits to a **new branch** on the remote (host-side),
   then open a PR yourself on your Git host.
8. **Publish or rebase** (optional): merge the latest remote sync branch into
   the work and push it back, or rebase the work onto it locally. See
   [Publish & Rebase](#publish--rebase).

## Endpoints

| Step | Endpoint | Where it runs |
|---|---|---|
| Credentials | `POST` / `GET /v1/git-credentials`, `DELETE /v1/git-credentials/{id}` | control plane |
| Import (public) | `POST /v1/apps` with `git: {repo_url, branch}` (no credential) | clone host-side, tokenless |
| Import (private) | `POST /v1/apps` with `git: {repo_url, branch, credential_id}` | clone host-side, via PAT |
| Runtime detect | `GET /v1/apps/{id}/runtime-inspect` | control plane (read-only) |
| Status | `GET /v1/apps/{id}/git/status` | **in-sandbox** |
| Diff | `GET /v1/apps/{id}/git/diff?path=` or `?range=origin/<sync branch>..HEAD` | **in-sandbox** |
| Commit | `POST /v1/apps/{id}/git/commit` | **in-sandbox** |
| Push | `POST /v1/apps/{id}/git/push` | control plane (host-side) |
| Sync (publish / rebase) | `POST /v1/apps/{id}/git/sync` | fetch + push host-side; merge/rebase **in-sandbox** |
| Abort sync | `POST /v1/apps/{id}/git/sync/abort` | **in-sandbox** |

All are owner/tenant-scoped (`auth.Actor.Name`): a caller only ever sees and acts
on its own apps and credentials; cross-owner access returns `404`.

---

## Execution model: who runs git, and why

The split is deliberate and is the heart of the security model:

- **Network git (clone, fetch, push) runs host-side in the control plane.** It needs
  the credential and the network — and the sandbox has neither (it runs with **no
  network**, and the token must never enter it). The token is decrypted only in
  these paths and delivered to git only via `GIT_ASKPASS` (see below).
- **Local git (status, diff, commit, merge, rebase) runs in-sandbox** via `docker exec`
  (uid 1000, locked container). It is **credential-free and offline** — it only reads
  and writes the workspace. Running it in the sandbox keeps an *untrusted* `.git`
  (the agent can write anything there, including hooks and `.gitattributes` that
  can execute programs) inside the existing trust boundary, instead of executing
  it on the control-plane host.
- **Runtime detection** reads workspace files host-side, read-only — it never runs
  app code, installs dependencies, or touches a credential.

## Runtime detection (advisory)

`GET /v1/apps/{id}/runtime-inspect` summarizes any existing `sandbox.yaml`
(authoritative, never overwritten), suggests a stack from the advisory recipe
registry (runnable presets `nextjs`/`react-vite`/`node-express`/`fastapi`/`worker`,
or detect-only recipes like `astro`/`docusaurus`/`gatsby`/`nuxt`/`sveltekit`/
`vite-vue`/`remix-vite`/`eleventy`), and for detect-only stacks returns a
`suggested_manifest` + `config_snippets`. It is purely advisory: **for an imported
repo sandboxd does not write `sandbox.yaml` or rewrite framework config** — you
adopt the suggestion explicitly (Copy YAML / Ask agent / manual edit). See
[`web-framework-recipes.md`](./web-framework-recipes.md).

Preview-port note: **runtime recipes standardize on `3000`, but a manifest can
declare any `web.port` and sandboxd routes the declared preview port.** The
resolved port is the imported `sandbox.yaml` `web.port`, else the selected preset,
else `3000`, and it drives both the Traefik router and the preview URL (so an
imported app on, say, 4321 previews correctly once its manifest declares it).

**Restart on change:** the manifest is read at boot, so **after you change
`sandbox.yaml`, restart the sandbox for runtimed to re-read it.**

## Status classification

`git/status` keeps `clean` truthful (raw repo state) and additionally reports
`user_clean` plus a separate `runtime_files[]` bucket, so a pristine import isn't
shown as "dirty" just because sandboxd/the toolchain wrote `sandbox.yaml`, a
lockfile, or a framework cache. Nothing is hidden — runtime-generated files are
surfaced separately, and the console renders them collapsed and unchecked.

## Commit

`git/commit` stages **only the selected, actually-changed paths** (defaults to the
user files from `git/status`; runtime files are excluded unless explicitly listed
in `runtime_paths`). It **never** uses `git add -A`, makes a path-scoped commit so
any changes the agent pre-staged are not swept in, runs with `--no-verify` (so an
untrusted repo hook can't run), and sets an **ephemeral author** via `git -c
user.name= -c user.email=` — never written to `.git/config`.

**First-commit limitation.** `git/commit` expects an **existing HEAD** — it handles
*subsequent* commits, not repository initialization. A brand-new repo with no commits
returns `reason:"empty_repo_unsupported"`. The **initial commit must be made
in-sandbox** for now (e.g. the agent runs `git init && git add -A && git commit` in
the workspace); after that, `git/commit`/`git/push` work normally. First-commit
support via the API is intentionally out of scope here.

## Push

`git/push` pushes `HEAD` to a **new branch** (default `sandboxd/<slug>-<shortsha>`)
on the remote. Key properties:

- The push **target comes from app metadata** (`app.git_repo_url` +
  `app.git_credential_id`), **never** the repo's `.git/config` origin.
- The credential reaches git only via a **host-checking** `GIT_ASKPASS`: the token
  is emitted only when git's credential prompt resolves to the **exact** expected
  host; any mismatch or unparsable prompt emits nothing.
- A **pre-flight config audit refuses** a repo whose local config has any key outside
  the allow-list (see Security boundaries), e.g. `insteadOf`, `hooksPath`, proxy, LFS
  or submodule config (`reason:"unsafe_repo_config"`).
- The invocation is hardened: hooks disabled, `protocol.ext/file.allow=never`,
  `GIT_ALLOW_PROTOCOL=https`, clean `HOME`/global/system config, no
  `credential.helper`, argv-only.
- **New branch only** (`main`/`master` and the import branch are rejected),
  **never `--force`**, no PR creation.
- It **wakes a stopped sandbox first** (repo state and the unpushed count are queried
  in the sandbox, and the host-side push runs with it paused), and refuses with
  `no_local_commits` when there's nothing to push (computed locally, no network).

## Publish & Rebase

Two sync actions share one endpoint, `POST /v1/apps/{id}/git/sync` with
`{"mode":"publish"}` or `{"mode":"rebase"}`. The console Git panel exposes them as
**Publish** and **Rebase on <branch>**. Any other `mode` returns `400`.

Both act on the app's **sync branch**: the branch the app was imported from
(`git.branch` on `POST /v1/apps`). An app created without `git.branch` is imported
from `SANDBOXD_SYNC_BRANCH` (control-plane environment), or `main` when that is
empty, and that becomes its sync branch. `GET /v1/apps/{id}/git/status` reports it
as `sync_branch`, and every sync response carries it as `branch`.

- **Publish** merges the latest remote sync branch into your work and pushes the
  result back to it. Fast-forward only, never `--force`.
- **Rebase** rebases your work onto the latest remote sync branch. It stays
  local; nothing is pushed.

Both modes start the same way:

1. **Pre-flight** — the same guards as push (repo URL, credential, workspace,
   `HEAD`, config audit). If a merge or rebase is already in progress, the call
   returns `state:"resolving"` with the existing `resolve_task_id` and starts
   nothing new.
2. **Auto-commit** — if user files changed, all of them are committed first
   (runtime files excluded), as `sandboxd: auto-commit before publish` or
   `… before rebase`. The short sha comes back as `auto_committed`.
3. **Fetch** — host-side, the sync branch is fetched into
   `refs/remotes/origin/<branch>` from the app's repo URL (never `.git/config`
   origin). On the first sync of a shallow clone this also unshallows the repo. A
   missing remote branch returns `no_main_branch`.
4. **Integrate** — in the sandbox: `git merge --no-edit origin/<branch>` (publish) or
   `git rebase origin/<branch>` (rebase).
5. **Outcome**:
   - `up_to_date` — nothing new on the sync branch, and (publish) nothing to push.
   - `rebased` — rebase finished cleanly; `commits` is the local commit count.
   - `published` — merge finished cleanly and the fast-forward push was accepted.
     If the branch moved in the meantime, the push fails with
     `non_fast_forward`: click **Publish** again.
   - `resolving` — the merge or rebase hit conflicts. See below.

### Conflicts and the agent hand-off

On conflict, sandboxd leaves the merge or rebase in progress, records the unmerged
paths, and submits an **agent task** through the same path as
`POST /v1/sandboxes/{id}/tasks`. The task gets the operation, the conflicting files,
and these instructions: resolve every conflict marker keeping the intent of both
sides, run the project's build or tests if available, `git add` the files, and
finish the operation (`git commit --no-edit` for a merge, `git rebase --continue`
until the rebase completes). The agent is told not to push, reset, or abort.

The task id is stored on the app, so `git/status` can return `in_progress`,
`conflicts`, and `resolve_task_id` after a page reload. If no agent is runnable, the
operation is aborted and the call returns `failed` with `agent_unavailable`.

> The conflict resolver task (`submitResolveTask`, `control-plane/internal/api/v1_git_sync.go`)
> submits only a prompt. It therefore runs on the operator default agent
> (`SANDBOXD_DEFAULT_AGENT`) and ignores configured model providers. With
> `SANDBOXD_DEFAULT_AGENT=pi`, the task is rejected (400 "select a model provider
> connection for Pi in Settings") and sync falls back to the failed /
> `agent_unavailable` path with no resolver task.

### Review gate

An agent-resolved merge is **committed locally and never pushed automatically**. To
review it, open the diff for `GET /v1/apps/{id}/git/diff?range=origin/<sync branch>..HEAD`,
which shows exactly what Publish would push. Clicking **Publish** again re-fetches,
merges nothing if the sync branch hasn't moved, and pushes. A rebase result is always
local-only; a later Publish is the only path to the sync branch.

While a merge or rebase is in progress, `git/commit`, `git/push`, and `git/sync`
refuse with `merge_in_progress` (sync returns `resolving` as above).

### Abort

If the agent can't resolve the conflicts, call
`POST /v1/apps/{id}/git/sync/abort`. It runs `git merge --abort` or
`git rebase --abort` under the workspace lock and does not touch the remote. With
nothing in progress it returns `{"aborted":false,"reason":"nothing_in_progress"}`.

### Why merge and rebase run in the sandbox

Merge drivers, clean/smudge filters, and hooks in a repo's config can execute
programs. If merge ran host-side, that code would run on the control plane. Running
it in the sandbox keeps it in the same trust boundary as the other in-sandbox git
commands. Only the fetch and the push run host-side, and they run without executing
repo-controlled code (hooks are disabled, see [Push](#push)).

## Concurrency

Commit, push, and sync take the same **per-workspace exclusive lock** (`git:<sandbox-id>`
— per workspace, not global, so different apps never block each other). The loser
of a commit race re-evaluates under the lock and returns
`{committed:false, reason:"no_changes"}` instead of a scary error; push holds the
lock across its whole pre-flight so it can't publish a stale `HEAD`. Sync holds the
lock through its pre-flight, auto-commit, fetch, and in-sandbox integrate steps, then
releases it before the agent task runs, so the agent never blocks commit or push
for the duration of the resolution. Read-only `status`/`diff` stay unlocked.
Workspace purge also acquires this lock before deleting the workspace dir, so it
can't race an in-flight commit/push/sync.

---

## Security boundaries

- **Token at rest** — encrypted (AES-GCM), owner-scoped, **write-only** through the
  API (`token_set` is the only field ever returned). Decrypted **only**
  control-plane-side, in the clone, fetch, and push paths.
- **Rotating a token** — the token is write-only, so there's **no in-place edit**.
  To rotate, **delete the credential and add it again** with the new token
  (`DELETE /v1/git-credentials/{id}` then `POST /v1/git-credentials`, or **Settings →
  Git credentials** in the console).
- **Token in motion** — reaches git solely via `GIT_ASKPASS` reading it from an
  inherited pipe (fd 3); it is never written to a file. It is **never** present in:
  argv (URLs are tokenless), the git process environment, `.git/config`,
  the workspace, snapshots, `docker inspect`, logs, or events. Fetch and push both
  use the host-checking askpass so the token can't be exfiltrated to a rewritten
  host.
- **The sandbox never sees the token.** Network git is host-side; the sandbox has
  no network and no credential.
- **Untrusted `.git`** — in-sandbox git is hardened (`-c core.fsmonitor=`,
  `--no-ext-diff --no-textconv`, `-c safe.directory=*`); merge and rebase add
  `--no-verify` and an empty `core.hooksPath`. Neither trusts the repo's origin remote.
- **Host-side fetch/push on an agent-controlled `.git`** — hardened in place:
  - **Surface**: the only host git is the fetch and the push themselves (plus their
    `rev-parse --absolute-git-dir` and config audit), all inside the pause. Every
    other read (repo state, unpushed count, ahead/behind) runs in the sandbox, and
    its answers can only make sandboxd refuse.
  - **Layout check** (pure Go, before any git): `workspace/app` and `.git` must be
    real directories (no symlink, no `gitdir:` file); `config`, `HEAD`, `packed-refs`,
    `shallow`, `refs/…`, `objects/…`, `logs/…` and every path component of
    `refs/remotes/origin/<branch>` and `logs/refs/remotes/origin/<branch>` for the
    branch about to be fetched or pushed (and `main`) must not be symlinks; `objects/info/alternates`, `commondir`, `config.worktree`, and any
    `[include]`/`[includeIf]` section or `worktreeConfig` in `.git/config` are refused. Git is pinned with
    `GIT_DIR`/`GIT_WORK_TREE`/`GIT_CEILING_DIRECTORIES` and
    `rev-parse --absolute-git-dir` must match.
  - **Allow-list audit** (`unsafe_repo_config`): every `.git/config` key must be one a
    clone or `git init` writes (`core.*` basics, `remote.origin.url|fetch`,
    `branch.<name>.remote|merge`, `user.*`, `init.defaultBranch`, `pull.rebase`,
    `advice.*`, `extensions.objectFormat`). Includes, `extensions.worktreeConfig`,
    any other `remote.*`, and all `http.*`/`url.*`/`credential.*` keys are refused —
    so repos with LFS, `filter.*`, `core.hooksPath` or `submodule.*` config get
    `unsafe_repo_config`. Fetch and push re-run the check and audit themselves right
    before the network op.
  - **`-c` overrides** on every host git: hooks off, https only, no credential
    helper/askpass/fsmonitor/sshCommand/proxy, `core.alternateRefsCommand`
    neutralized, fixed `user.name`/`user.email`, fsck on, objects always kept as a
    pack, no auto-gc/maintenance, no reflog, no submodules; the env sets
    `GIT_NO_LAZY_FETCH=1` and an empty `GIT_ASKPASS` (ours only where a token is
    needed). Fetch adds `--no-write-fetch-head --no-auto-maintenance`. Host git older
    than 2.45 is refused (`host_git_too_old`).
  - **Pause**: the sandbox (woken first if stopped) is `docker pause`d around audit +
    fetch and audit + push, so the agent can't rewrite `.git` in between (always
    unpaused; never held across an in-sandbox exec).
  - **Unprivileged**: when the control plane is root, host git runs as the uid/gid
    owning `.git` (the sandbox user; uid 1000 if root-owned), with no supplementary
    groups.
  - **Residual risk**: this is list-based hardening of a tool that reads attacker
    input; a git config key or repo file we haven't anticipated could still change
    behaviour. The long-term fix is a host-owned bare repo (or bundle exchange), so
    host git never opens the agent's `.git`.
- **Writes to the sync branch** — only `git/sync` with `mode:"publish"` writes the
  app's sync branch (its import branch), and only fast-forward. The remote enforces
  this; sandboxd never sends `--force`.
- **Accepted risks** — auto-commit publishes whatever user files exist in the
  workspace, and there is no secret scanning. Publish pushes to the sync branch with
  no review step beyond the Publish click; if the app was imported from a protected
  or release branch, Publish writes that branch (use branch protection on the host,
  or a PAT without write access, to require review).
- **Events/audit** carry repo URL (tokenless) + branch + result only.

## Limitations (current)

- **HTTPS + PAT only** — no SSH, GitHub/GitLab App, OAuth, or provider APIs.
- **Shallow clone** (`--depth=1`). The first Publish or Rebase on a shallow clone
  unshallows it. The Push button still returns `shallow_push_unsupported` if a remote
  rejects a push for shallowness.
- **Push is new branch only** — `git/push` never targets `main`/`master` or the
  import branch. Only `git/sync` with `mode:"publish"` writes the sync branch, and only
  fast-forward. **No PR creation** (open the PR yourself on your Git host).
- **Sync target is the import branch** — there is no separate per-app target branch;
  to publish elsewhere, import the app from that branch.
- **Empty repos** (no `HEAD`) can't be committed, pushed, or synced in this slice.
- **status/diff/commit need a running sandbox**; push and sync wake a stopped
  sandbox first.
- **Fork/restore degrades Git lineage** — snapshot spin-up resets the workspace
  `.git`. The push target survives (it's app metadata), but on a forked/restored
  app the local history/baseline is degraded; the direct *import → edit → commit →
  push* path is the supported one.
- **No secret scanning** of committed/pushed content — pushing publishes whatever
  is in the selected files. The new-branch default and it being your own private
  repo limit the blast radius.

## Explicit non-goals

No PR creation, no repo discovery/picker, no push to `master` or the import branch
via `git/push`, no configurable sync target, no force push, no `git pull`, no
auto-push after an agent resolution, no GitHub/GitLab API calls, no SSH/App/OAuth,
no secret scanning. These are out of scope, not pending.
