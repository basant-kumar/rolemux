# RoleMux v1 implementation plan

Status: approved for implementation by the orchestrator after the Sol reviewer
hit its recorded usage limit during the final re-review. Earlier Sol findings
were resolved and independently revalidated against the installed CLIs and
pinned SDK. This plan is the source of truth for the thin macOS-first Go CLI.
It intentionally keeps provider orchestration, durable task state, and host
integration small and testable. No license file is to be added.

## 1. Product boundary

RoleMux is a host-facing CLI that runs a durable four-role coding workflow:

1. `planner` turns a user task into a plan;
2. `plan_reviewer` reviews that plan;
3. `implementer` makes the requested changes;
4. `code_reviewer` reviews the resulting scoped changes and can return findings
   to the implementer for a bounded review loop.

The host/orchestrator decides which task IDs run and whether independent tasks
run in parallel. RoleMux intentionally runs all tasks in the existing checkout:
it does not create worktrees, stash, revert, commit, merge, rebase, reset, push,
or impose a global agent/concurrency cap. A task may declare a repository-
relative scope as paths or globs. Scope overlap is reported as an advisory; it
does not override the orchestrator's concurrency decision.

The application is macOS-first but uses portable Go process, filesystem, JSON,
and `golang.org/x/sys/unix.Flock` advisory-lock primitives. Provider-specific
behavior is isolated behind fakes in tests. Paid model calls are never used by
tests.

## 2. Repository layout

```text
go.mod                           module and pinned provider dependencies
cmd/rolemux/main.go              process entry point and exit handling
internal/config/                  layered TOML and environment configuration
internal/catalog/                live-first model catalog and scoped cache
internal/picker/                 searchable arrow-key model/effort picker
internal/install/                atomic conflict-safe host skill installer
internal/runner/                 provider adapters and subprocess safety
internal/provider/               canonical provider IDs
internal/task/                   state, manifest, scope, and task locks
internal/workflow/               phase machine and persistent review loops
internal/cli/                    command parsing, JSON contract, diagnostics
SKILL.md                          portable root host skill
internal/install/assets/SKILL.md  embedded installer asset; byte-identical
agents/openai.yaml                host skill metadata and $rolemux prompt
README.md                         install, config, security, and workflow docs
```

Runtime state is stored below `git rev-parse --git-path rolemux` (normally
`.git/rolemux`), never in tracked source. Human-readable task plans are stored
at `<repo>/.rolemux/plans/<task-id>.md` because they are project artifacts; each
plan is written through a same-directory temporary file, fsync, and atomic
rename. `.rolemux/**` plan artifacts and private git state are excluded from
the default `**` implementation scope. If the working directory is not a Git
checkout, startup fails closed because scope and durable state cannot be made
repository-relative. Source is implemented and delivered directly in
`/Users/basant/workspace/rolemux`; temporary directories are used only for
disposable build caches and binaries.

## 3. CLI surface and exact commands

The parser accepts one hierarchical command and optional `--json`; JSON never
starts an interactive picker. These spellings are part of the public contract:

```text
rolemux version [--json]
rolemux models [--refresh] [--runner codex|claude|copilot] [--json]
rolemux configure [--global|--project] [--from PATH|-] [--role ROLE]
                  [--runner PROVIDER] [--model MODEL] [--effort EFFORT]
                  [--json]
rolemux plan start --task TEXT [--id TASK-ID] [--json]
rolemux plan answer TASK-ID --answer TEXT [--json]
rolemux plan review TASK-ID [--json]
rolemux implement TASK-ID [--scope PATH[,PATH...]] [--json]
rolemux implement answer TASK-ID --answer TEXT [--json]
rolemux code review TASK-ID [--json]
rolemux status TASK-ID [--json]
rolemux retry TASK-ID [--json]
rolemux list [--json]
rolemux doctor [--json]
rolemux install --global --hosts all|claude,codex,copilot [--force] [--json]
```

`TASK-ID` is a single path-safe slug (`[A-Za-z0-9._-]{1,128}`), never a path.
An explicit duplicate task ID is rejected. When `--id` is omitted, `plan start`
generates a short time/random slug and checks it under the task lock before
use. `--task` is required and is stored verbatim as the user task input; it is
not taken from a positional argument or a provider prompt assembled by the
shell.

`plan start` stores the task before the planner provider call. `plan review`
may be run only after a plan is available. `implement` may be run only after
plan approval, and atomically establishes scope and baseline on its first call.
`code review` may be run only after implementation is ready. `retry` is legal
only from a durable retryable failure or `review_needed`; it repeats the exact
failed role/operation in the saved provider session. `status`, `list`, and
`models` are read-only. `configure --global` writes
`~/.config/rolemux/config.toml`; `configure --project` writes
`<repo>/.rolemux.toml`. `--global` and `--project` are mutually exclusive;
omitting both selects project configuration only when the command is
interactive and confirms the repository, otherwise it is a usage error.

Configuration has one shared `[profiles.reviewer]` default used by both review
roles, plus optional `[profiles.plan_reviewer]` and
`[profiles.code_reviewer]` overrides. The effective four-role snapshot always
contains planner, plan reviewer, implementer, and code reviewer; an absent
specific override is expanded from the shared reviewer profile before task
creation. Noninteractive configure with `--role` selects exactly one named
profile per invocation: `planner`, `implementer`, `reviewer`, `plan-reviewer`,
or `code-reviewer`. The common `--runner`, `--model`, and `--effort` flags
atomically update only that profile; repeated invocations configure the other
profiles. `--from PATH` reads a complete TOML config fragment from PATH;
`--from -` reads it from stdin. `--from` is mutually exclusive with role
selection flags, is never interpreted as executable code, and atomically
imports the complete RoleMux-owned profile/provider/model tables while
preserving unrelated keys. An interactive TTY walks the full set—planner,
implementer, shared reviewer, optional plan/code overrides, model, and optional
effort—using the live picker. `configure` has no `--force`; an existing file is
normally updated in place semantically, not rejected merely because it exists.
The command records the file hash before reading, rechecks it immediately
before the atomic write, and reports a concurrent-modification conflict only
when that hash drifted. No existence-only conflict is reported.

## 4. Durable state and phase machine

Each task JSON contains at least:

```json
{
  "id": "task-123",
  "repo_root": "/repo",
  "phase": "implementation_ready",
  "round": 0,
  "task": "...",
  "prompt": "...",
  "plan": "...",
  "plan_hash": "sha256",
  "approved_plan_hash": "sha256",
  "approved_manifest_hash": "sha256",
  "planner_session_id": "...",
  "plan_reviewer_session_id": "...",
  "implementer_session_id": "...",
  "code_reviewer_session_id": "...",
  "scope": "internal/cli,internal/workflow",
  "scope_spec_hash": "sha256",
  "scoped_baseline_manifest": [],
  "scoped_baseline_manifest_hash": "sha256",
  "change_manifest": [],
  "profiles_snapshot": {
    "planner": {"provider":"codex","model":"...","effort":"high"},
    "plan_reviewer": {"provider":"claude","model":"...","effort":"high"},
    "implementer": {"provider":"codex","model":"...","effort":"high"},
    "code_reviewer": {"provider":"claude","model":"...","effort":"high"}
  },
  "runtime_snapshot": {
    "planner": {"provider_type":"codex","provider_id":"...","endpoint":"...",
                 "wire_api":"...","auth_env_refs":[],"auth":{},
                 "cli_path":"/opt/homebrew/bin/codex","sdk_settings":{}},
    "plan_reviewer": {"provider_type":"claude","provider_id":"...","endpoint":"...",
                       "wire_api":"...","auth_env_refs":[],"auth":{},
                       "cli_path":"/opt/homebrew/bin/claude","sdk_settings":{}},
    "implementer": {"provider_type":"codex","provider_id":"...","endpoint":"...",
                    "wire_api":"...","auth_env_refs":[],"auth":{},
                    "cli_path":"/opt/homebrew/bin/codex","sdk_settings":{}},
    "code_reviewer": {"provider_type":"claude","provider_id":"...","endpoint":"...",
                       "wire_api":"...","auth_env_refs":[],"auth":{},
                       "cli_path":"/opt/homebrew/bin/claude","sdk_settings":{}}
  },
  "max_rounds": 5,
  "plan_round": 0,
  "code_round": 0,
  "pending_question": "",
  "pending_question_source": "",
  "return_phase": "",
  "interrupted_loop": "",
  "findings": [],
  "advisories": [],
  "in_flight": null,
  "retry": null,
  "updated_at": "RFC3339"
}
```

The implementation may add diagnostic compatibility fields, but must not store
tokens, API keys, or raw credential values. `profiles_snapshot` is copied at
task creation and is the only profile source for all later turns; it contains
all four provider/model/effort choices and `max_rounds` fixed at 5. The
`runtime_snapshot` separately persists, per role, provider type/id, endpoint,
wire API, safe auth environment references/auth-command metadata, resolved CLI
path, and non-secret SDK/provider settings. Existing tasks use these snapshots
on fresh, resume, and retry; they never reread changed current configuration.
Credential values are reacquired from the snapshotted environment references or
provider auth store each turn; missing values are actionable failures. Plan and
code counters are separate. `pending_question` stores the exact question,
`pending_question_source` identifies planner or implementer, and
`return_phase`/`interrupted_loop` preserve the phase and loop that must resume
after an answer or process restart. `in_flight` records an unguessable operation
token, operation/role, previous phase, prompt/findings/scope, provider session
knowledge, the exact provider session ID, and the provider-turn pre-snapshot.
`retry` preserves the exact operation, prompt, findings, scope, and session
needed to resume without re-resolving current configuration.

Legal transitions:

```text
planned -> plan_reviewing | needs_input | failed
plan_reviewing -> plan_approved | planned (changes loop) | needs_input | failed
plan_approved -> implementing
implementing -> needs_input | implementation_ready | failed
needs_input -> planned (plan answer) | implementing (implementation answer)
implementation_ready -> code_reviewing
code_reviewing -> approved | implementing (finding loop) | failed
any provider turn -> failed (known-session retryable) | failed (unknown-session
                         fail-closed)
code_reviewing with scoped barrier mutation -> review_needed
review_needed -> code_reviewing only through retry
```

The planner, plan reviewer, implementer, and code reviewer are persistent
conversations. A plan answer always resumes the SAME planner session. A plan
review finding is sent to that SAME planner, then the updated plan is sent to
the SAME plan reviewer. A code finding is sent to the SAME implementer, then
the candidate is sent to the SAME code reviewer. Separate `plan_round` and
`code_round` counters are incremented only after valid envelopes are accepted;
the fifth exhausted plan or code round returns exit 7. A stale reviewer verdict is
discarded and does not consume a round. Provider errors in the initial planner
turn are retryable whenever the provider emitted and durably persisted a usable
session ID; an unknown session is never guessed or silently restarted.

Codex and Claude turns request and validate a provider-native JSON schema;
Copilot planning/review uses the exact-one-object prompt/decoder described in
section 7 because the pinned SDK has no native schema option. The only accepted
envelopes are:

```json
{"role":"planner","status":"ready","plan":"..."}
{"role":"planner","status":"needs_input","question":"..."}
{"role":"plan_reviewer","verdict":"approved","findings":[]}
{"role":"plan_reviewer","verdict":"changes_requested","findings":[{"path":"...","line":1,"severity":"...","message":"..."}]}
{"role":"implementer","status":"ready"}
{"role":"implementer","status":"needs_input","question":"..."}
{"role":"code_reviewer","verdict":"approved","findings":[]}
{"role":"code_reviewer","verdict":"changes_requested","findings":[{"path":"...","line":1,"severity":"...","message":"..."}]}
```

Unknown fields, missing required fields, multiple JSON values, markdown fences,
or prose without a valid envelope are provider errors; RoleMux never infers a
verdict from prose. Questions and repeated answers are persisted as task
inputs and included in the next same-session prompt. A planner may transition
directly from `planned` to `needs_input`; after `plan answer`, the saved
`return_phase` and loop identity restore the planner turn and, when applicable,
the same plan-reviewer turn. Both plan and code loops stop at five accepted
review cycles with exit 7.

An interruption at any point writes `retry` before returning: it records the
loop (`plan_initial`, `plan_review`, `implement`, or `code_review`), the
previous phase, exact provider role/session, pending question or findings,
prompt inputs, and both counters. A restart/retry reconstructs that loop from
state rather than starting a second session. A planner `needs_input` response
persists `pending_question_source="planner"` and `return_phase`; an
implementer question uses `pending_question_source="implementer"`. Repeated
answers append to the saved prompt-input history and resume the same session.

## 5. Same-checkout concurrency and ownership

There is no repository-global clean-worktree or task lock. A task advisory lock
is a persistent lock file acquired with ownership-safe
`golang.org/x/sys/unix.Flock`; it is never unlinked by cleanup. The lock is held only
for short atomic state load/validate/update/write operations. Provider calls,
manifest hashing, and user interaction happen without the lock.

Beginning an operation performs a short compare-and-swap against the latest
state, rejects an existing `in_flight` operation for that task, records a new
operation token and pre-call metadata, then releases the lock. Every callback
and completion writes through token-checked CAS. A stale completion returns
`STALE_OPERATION` and cannot overwrite newer state.

Two disjoint task IDs must be able to enter long fake/provider calls at the
same time in one checkout. A second mutation command on the same task must be
rejected while its provider operation is in flight. The tests use barriers to
prove both properties and prove that stale completion cannot win.

The orchestrator owns the risk of overlapping same-scope writers: RoleMux does
not claim attribution and does not serialize them. It emits structured
`SCOPE_OVERLAP`, `SCOPE_UNMATCHED`, and `OUT_OF_SCOPE_CHANGE` warnings. These
warnings never stash, revert, commit, merge, or globally block another task.

## 6. Canonical scopes and exact content manifests

`--scope` is comma/newline separated, normalized to slash paths, sorted and
deduplicated. It rejects absolute paths, `~`, NUL, backslash escapes, and any
`..` traversal. Empty scope canonicalizes to `**`, whose matcher explicitly
excludes `.rolemux/**` project plan artifacts and all `.git/**` private state.
An explicit project scope may include a tracked `.rolemux/plans/...` artifact,
but private git state is never addressable. The canonical string is persisted
with `sha256(scope-spec)` and cannot change on later commands for that task.

On the first `implement` invocation, scope and its exact baseline are
established in one task-state mutation. The current scoped content is accepted
as baseline even if initially dirty; no clean-worktree requirement exists.

The manifest is a genuine HEAD-independent exact content/index observation, not
a `git status` or HEAD-derived fingerprint. It is sorted canonical JSON of
repository-relative entries with this shape:

```json
{
  "path":"internal/cli/cli.go",
  "kind":"file",
  "worktree":{"present":true,"mode":"0755","hash":"sha256","size":1234},
  "index":{"present":true,"mode":"100755","blob":"git-blob-sha",
            "stages":[0]},
  "content_ref":"baseline/task-123/sha256"
}
```

`worktree` is computed by `lstat` and hashes bytes without following symlinks;
`index` is read from `git ls-files --stage` and the blob object. `stages`
preserves all unmerged stages. `content_ref` points to a private durable blob
copy, never a tracked path. Presence, kind, worktree mode/hash/size, index blob,
index mode, and stage set are the approval fingerprint. Derived Git status
labels and HEAD/branch are advisory metadata only and are excluded from the
fingerprint. It must include:

- tracked, untracked, modified, staged, and deleted paths;
- symlinks hashed from their link target without following them;
- submodules (mode `160000`) as submodule entries;
- directories and parent directories where present;
- executable and other mode-only changes.

Deleted paths remain entries with `worktree.present=false` and retain index
blob/mode/stages. Renames are represented as a removed old path plus an added
new path; a derived rename label is advisory. Ignored files and private state
are excluded. The manifest includes structural directory ancestors needed to
locate scoped paths, but its fingerprint projection includes only
scope-relevant descendant edges/content. An ancestor entry contributes its
mode/content only when the directory itself is in scope; otherwise its
structural edge list contains only in-scope descendants. Unrelated siblings
therefore never alter a scoped hash. Submodules preserve their index `160000`
gitlink object and worktree kind.
Hashing follows no symlink outside the repository. `HashManifest` hashes only
the canonical exact fields above after the scope projection, so meaningful
scope-relevant content/index/mode changes change the fingerprint even when HEAD
and derived status do not.

At baseline establishment, RoleMux copies recoverable worktree bytes/link
targets and index blob bytes into `git rev-parse --git-path rolemux` under a
task-specific, content-addressed baseline blob directory. Baseline refs are
immutable: an existing digest is verified and never overwritten. After each
implementer turn, candidate content is copied into a distinct content-addressed
candidate directory; candidate refs may not overwrite baseline refs or an
earlier candidate. The state record stores digest, size, mode, kind, and
relative blob reference. This preserves initially dirty, staged/unstaged,
untracked, deleted, renamed, symlink, submodule, and mode-only baseline
content after any HEAD or branch movement. Blob writes happen before the short
state CAS; a failed blob write aborts the state mutation. No content is read
from a future HEAD to reconstruct a baseline.

The reviewer receives only the task's scoped baseline-to-candidate diff/status
constructed from the private baseline content refs and current scoped content
refs. It never receives the whole-repository `git diff`, a whole-repository
status, or an unbounded host path. Before a code-review provider call, after
scope exists, RoleMux snapshots the scoped manifest immediately before the
read-only reviewer and compares it immediately after. Any scoped change
returns `REVIEW_NEEDED` (exit 4), leaves the reviewer round/verdict
unconsumed, and preserves the reviewer session for `rolemux retry TASK-ID`.
Plan review is bound only to the plan hash and has no worktree barrier.

After an implementer turn, the resulting scoped manifest is accepted as the
candidate. Out-of-scope movement is advisory and does not stale this task's
approval. A subsequent code-review approval binds
`approved_manifest_hash` to the current scoped manifest; later approval reuse
revalidates plan hash, scope-spec hash, baseline hash, and approved scoped
manifest hash.

## 7. Provider adapters and permission profiles

All adapters implement a common request/response interface with context,
operation, role, prompt, model, optional effort, repository root, scope,
session ID, and callbacks for durable session-start events. Provider responses
include text, session ID, reported model/effort where available, and structured
errors. A fake adapter is injectable into workflow tests. Provider construction
is centralized in a registry, so catalogs, pickers, workflows, and CLI commands
do not need new provider-specific branches when another CLI adapter is added.

Initial role prompts include the complete required context and explicit token
discipline. Resumed turns send only changed inputs (answers, findings, revised
plans, and scoped candidate deltas), relying on the exact persisted provider
session. RoleMux never truncates required context or substitutes a lossy summary.

### Codex

The implementation is pinned and validated against Codex CLI `0.153.3` (or a
newer version that passes the same capability probe). Resolve
`CODEX_CLI_PATH` or the installed `codex` executable with `exec.LookPath`;
failure is explicit. At startup/doctor, parse `codex --help`, `codex exec
--help`, and `codex exec resume --help`; do not assume a flag merely because a
different CLI release used it. The canonical invocation requires every option
before the final stdin marker and places resume flags before the session ID:

```text
codex -C REPO -s MODE -a never --search exec --ignore-user-config \
  --ignore-rules --json --model MODEL --output-schema SCHEMA \
  [--config model_reasoning_effort=EFFORT] -
codex -C REPO -s MODE -a never --search exec resume --ignore-user-config \
  --ignore-rules --json --model MODEL --output-schema SCHEMA \
  [--config model_reasoning_effort=EFFORT] SESSION -
```

Only `-C REPO`, `-s MODE`, `-a never`, and `--search` precede `exec`.
`--ignore-user-config` and `--ignore-rules` follow `exec` (and follow `resume`
in the resume form); all other resume flags accepted by `exec resume --help`
precede `SESSION`. The prompt is supplied on stdin as the final `-`, never
interpolated into argv. `--output-schema` is an explicit temporary schema file.
Implementation changes only `MODE` to
`workspace-write`; fresh and resume requests use the same security flags.
Effort is applied through the validated installed-CLI config form, and the
adapter verifies/reports model and effort fidelity from JSON events when
supplied. It never adds `--dangerously-bypass-approvals-and-sandbox`.

RoleMux must not blindly pass `--ignore-user-config` while claiming to preserve
gateway support. Before invocation, it reads `$CODEX_HOME/config.toml` and,
when configured, the explicitly selected named profile if that Codex release
supports profiles. It parses TOML into a typed/raw tree and extracts only this
allowlist: `model_provider`, `openai_base_url`, and selected
`model_providers.<name>` routing/auth-reference metadata (`base_url`,
`wire_api`, `env_key`, `env_http_headers`, `query_params`, request and stream
retry/timeout settings, `supports_standalone_web_search`,
`requires_openai_auth`, and nested `auth.command`, `auth.args`,
`auth.timeout_ms`, and `auth.refresh_interval_ms`). The `name` is the safe
provider identifier; it is not a credential.
The `auth.command`/`auth.args` argv is executed by Codex's own provider auth
machinery, never by RoleMux; RoleMux only validates and forwards its safe
metadata. Its command token must be a bare executable name or absolute path
without shell metacharacters, substitutions, redirection, or whitespace, and
its argument array must contain no shell syntax or literal bearer/API/token/
secret values. Unknown fields in the selected routing profile, literal
credentials, unsafe command/args, and values that would place a credential in
argv are actionable errors; there is no fallback to an unscoped user config.
Safe fields are serialized deterministically as explicit TOML
`-c key=value` arguments (strings quoted with TOML rules, maps sorted by key,
arrays canonicalized) and are reapplied alongside explicit model, effort,
sandbox, approval, search, and schema overrides. `CODEX_HOME` remains in the
sanitized child environment for auth/session state; `--ignore-user-config`
then prevents any other user/rules setting from entering the process.

The selected safe routing document is also written only to a mode-0600
temporary file under the private task runtime directory when the CLI requires
file-based config; it is fsynced, removed after process exit, and never logged.
RoleMux never invokes, captures, or logs an auth command's output; Codex owns
its execution and credential handling.
Serialization tests compare exact argv/config bytes and assert secret values
cannot appear. Unsupported or unsafe provider config fails closed and names the
field to fix without revealing its value.

JSONL stdout and stderr are drained concurrently. If stdout parsing or the
bounded output limit fails, the child is killed before `Wait` to avoid a pipe
deadlock; stderr is never read synchronously after stdout. A fresh Codex turn
must observe a durable `thread.started` event and its non-empty ID before a
successful response can be returned. Resume uses the exact saved ID and fails
closed if it cannot resume. Model discovery initializes the CLI and follows
every `model/list` page/cursor until completion.

### Claude

The implementation is pinned and validated against Claude Code `2.1.260` (or a
newer version that passes the same capability probe). Resolve
`CLAUDE_CLI_PATH` or `claude` from PATH. Use the actual case-sensitive tool
names and both restricted/preapproval surfaces. The fresh and resume forms are
distinct:

```text
claude --print --output-format json --input-format text --safe-mode --restricted \
  --permission-mode dontAsk --permission-prompts none \
  --tools Read,Glob,Grep,WebSearch,WebFetch[,Edit,Write] \
  --allowed-tools Read,Glob,Grep,WebSearch,WebFetch[,Edit,Write] \
  --strict-mcp-config --mcp-config '{"mcpServers":{}}' \
  --session-id UUID --json-schema '{"type":"object",...}' \
  --model MODEL [--effort EFFORT]
claude --print --output-format json --input-format text --safe-mode --restricted \
  --permission-mode dontAsk --permission-prompts none \
  --tools Read,Glob,Grep,WebSearch,WebFetch[,Edit,Write] \
  --allowed-tools Read,Glob,Grep,WebSearch,WebFetch[,Edit,Write] \
  --strict-mcp-config --mcp-config '{"mcpServers":{}}' \
  --resume SESSION --json-schema '{"type":"object",...}' \
  --model MODEL [--effort EFFORT]
```

Plan/review profiles include exactly `Read,Glob,Grep,WebSearch,WebFetch`;
implementation profiles add exactly `Edit,Write`. No Bash, shell, process,
arbitrary MCP, plugin, skill, or bypass flag is allowed. Every fresh session
gets a preassigned UUID through `--session-id`; resume uses that same ID and
preserves every isolation flag but does not pass `--session-id` again. The
prompt is piped through stdin as `--input-format text`; there is no positional
stdin marker or trailing `-`. `--json-schema` is an inline JSON schema string,
not a path. `Cmd.Dir` is the repository root. The adapter parses Claude's
terminal single JSON result in two steps: first decode the outer result wrapper,
require its exact `session_id` to equal the fresh preassigned or resumed ID,
then strictly decode its nested `structured_output` object as the role
envelope. The decoder rejects fences/prose/multiple values, missing wrapper or
structured-output fields, unknown envelope fields, and session mismatches; it
never treats the wrapper's human `result` string as a verdict.

Claude gateway/BYOK routing is carried by explicitly selected non-secret
provider configuration and sanitized allowlisted environment variables on both
fresh and resume invocations. Provider URL, provider name, auth-reference, and
model flags are rebuilt for every turn; API keys/tokens remain in the provider
credential store/environment and are never placed in argv or durable state.
Missing or unsafe routing configuration is actionable and fail-closed, not a
fallback to the user's unrestricted settings.

### Copilot

The minimum build is Go `1.24`. The pinned dependency is the official GitHub
Copilot Go SDK `github.com/github/copilot-sdk/go v1.0.13-preview.4`.
`NewCopilot` resolves
`COPILOT_CLI_PATH` first, then `exec.LookPath("copilot")`; it must never pass an
empty runtime path when the SDK requires a process path. For read-only
discovery/review experiments, configure `ModeEmpty`, a private persistent
base/session directory, disabled config/skill/plugin/hook/MCP discovery, an
explicit allowlist, and a permission callback. The exact pinned SDK tool
surface is `copilot.NewToolSet().AddBuiltIn("view", "grep", "web_fetch").ToSlice()`
(wire names `builtin:view`, `builtin:grep`, `builtin:web_fetch`); there is no
`web_search` tool in this SDK release. Fresh and resume sessions use the
identical list.

The permission handler is deny-by-default for every request kind. It approves
once only a `PermissionRequestRead` whose canonical/lstat-resolved path remains
beneath the repository, with `ManagedApprovalRequired != true` and
`RequestSandboxBypass != true`; it approves once only a `PermissionRequestURL`
for `http`/`https` URLs with no userinfo and the same two safety flags. Every
other kind (custom-tool, extension-*, factory, hook, mcp, memory, shell, write,
raw, or unknown), plus unsafe reads/URLs, returns
`&rpc.PermissionDecisionReject{Feedback: &msg}`. No MCP, hook, agent, shell,
or write request can be approved. Read path checks reject arbitrary host paths
and symlink escapes.

The pinned SDK does not provide a native output-schema contract equivalent to
the CLI adapters. Copilot planning/review therefore uses a prompt that demands
exactly one JSON object matching the role envelope in section 4. The adapter
parses the complete terminal result with a strict decoder, rejects markdown
fences, prose, trailing/multiple JSON values, unknown fields, and missing
fields. It never infers a verdict from a natural-language response.

Copilot implementation is rejected in v1 unless this pinned SDK/API is proven
to expose the complete approved write isolation, preassigned-session, schema,
and resume controls. If that proof is absent, selecting Copilot for the
implementer returns a structured `UNSUPPORTED_PROVIDER` error and exit 5; it
never falls back to an unconfined Copilot CLI invocation or weakens the
permission matrix. The limitation is documented in README and doctor output.

The SDK `Provider`/BYOK configuration, including safe provider name, endpoint,
wire mode, and environment-variable references, is supplied on every fresh and
resume `CreateSession`/`ResumeSession` call. Credentials are not persisted by
RoleMux and are never assumed to survive a process restart; the host must
reprovide them through the sanitized environment/provider callback each turn.

All provider environments are sanitized to remove bypass/auto-approve variables
while retaining provider authentication and non-secret gateway endpoint
variables. Diagnostics never print environment values.

## 8. Layered TOML configuration

Layers, in order:

1. built-in defaults (provider roles but no guessed model);
2. global `${XDG_CONFIG_HOME:-$HOME/.config}/rolemux/config.toml`;
3. project `<repo>/.rolemux.toml`;
4. explicit `ROLEMUX_CONFIG` if set (a replacement for layers 2–3);
5. command-line selections.

`configure --global` writes exactly the global path and `configure --project`
writes exactly `<repo>/.rolemux.toml`; neither command writes into the private
git state directory. The explicit file is the only replacement for the
global/project pair, not a secret store. Environment variables may select
executable paths and gateway endpoints but are never serialized or printed.
Profile keys and TOML names are fixed as follows; serialization uses sorted
tables and keys, canonical quoted strings, and a final newline:

```toml
[profiles.planner]
provider = "codex"
model = "gpt-5-codex"
effort = "high"

[profiles.reviewer]
provider = "claude"
model = "claude-sonnet"
effort = "high"

[profiles.implementer]
provider = "codex"
model = "gpt-5-codex"
effort = "high"

[providers.codex]
cli_path = "/opt/homebrew/bin/codex"
gateway_url = "https://gateway.example.invalid"

[providers.claude]
gateway_url = "https://anthropic-gateway.example.invalid"
api_key_env = "ANTHROPIC_API_KEY"
auth_token_env = "CLAUDE_CODE_OAUTH_TOKEN"
bedrock_profile_env = "AWS_PROFILE"
bedrock_region_env = "AWS_REGION"
vertex_project_env = "CLOUD_ML_PROJECT_ID"
vertex_region_env = "CLOUD_ML_REGION"
foundry_endpoint_env = "FOUNDRY_ENDPOINT"
foundry_api_key_env = "FOUNDRY_API_KEY"

[providers.copilot]
base_url = "https://api.githubcopilot.com"
type = "openai"
wire_api = "responses"
bearer_token_env = "GH_COPILOT_TOKEN"
transport = "http"
model_id = "gpt-5-codex"
wire_model = "gpt-5-codex"

[models.codex.local-review]
id = "local-review"
label = "Local Review"
aliases = ["review-local"]
efforts = ["low", "medium"]
default_effort = "medium"
availability = "unknown"
wire_api = "responses"
base_url = "https://gateway.example.invalid/v1"
env_key = "ROLEMUX_CODEX_KEY"

[models.claude.claude-fable-5]
id = "claude-fable-5"
label = "Claude Fable 5"
aliases = ["fable"]
efforts = ["low", "medium", "high"]
default_effort = "medium"
availability = "unknown"
```

Unknown roles/providers, negative catalog TTL, malformed TOML, and
credential-like fields are configuration errors. `configure` uses the live
catalog first and falls back to a clearly labeled cache only when live
discovery fails. A configuration write uses a mode-0600 same-directory temp
file, fsync, close, and atomic rename; existing unrelated TOML keys are
preserved and only the selected profile/provider fields change. Before writing,
the original file hash is compared with a fresh hash; only a changed hash is a
concurrent-modification conflict. An existing file by itself is not a conflict.

Runner gateway/BYOK tables use fixed non-secret fields. Claude accepts
`gateway_url`, `api_key_env`, `auth_token_env`, `bedrock_profile_env`,
`bedrock_region_env`, `vertex_project_env`, `vertex_region_env`,
`foundry_endpoint_env`, `foundry_api_key_env`, and an optional `env_refs` map
whose values are environment-variable names. These names are validated and
the corresponding values are reacquired from the sanitized environment on
every fresh and resume CLI invocation. The recognized Anthropic, Bedrock,
Vertex, and Foundry selectors are environment references, never literal
credentials; unsupported provider fields fail closed.

Copilot tables map exactly to SDK `ProviderConfig`/`NamedProviderConfig` safe
fields: `type`, `wire_api`, `transport`, `base_url`, `headers` (only
non-secret values), `model_id`, `wire_model`, `max_prompt_tokens`,
`max_output_tokens`, optional Azure `api_version`, and `bearer_token_env` or
an SDK bearer-token-provider reference. A named model additionally maps to
`ProviderModelConfig` fields `id`, `provider`, `wire_model`, `model_id`,
`name`, token limits, and capability overrides. Static API/bearer values are
never accepted in TOML. The complete Provider/NamedProvider/Model
configuration and credential callback is supplied again to every SDK
`CreateSession` and `ResumeSession` call, including after restart.

Custom model tables are provider-keyed at `[models.<provider>.<name>]` and must
contain `id`, optional `label`, sorted unique `aliases`, optional `efforts`,
optional `default_effort`, and `availability` (`available`, `unknown`, or
`unavailable`). Codex custom models may additionally contain only the safe
routing fields `name`, `base_url`, `wire_api`, `env_key`, `env_http_headers`,
`query_params`, `request_max_retries`, `stream_max_retries`,
`stream_idle_timeout_ms`,
`supports_standalone_web_search`, `requires_openai_auth`,
and nested `auth.command`, `auth.args`, `auth.timeout_ms`, and
`auth.refresh_interval_ms`; Claude custom models may contain only `aliases`,
`efforts`, and availability metadata because its v1 catalog has no
paid live endpoint; Copilot custom models are rejected unless SDK discovery
proves the provider. `claude-fable-5` is the canonical built-in Claude model ID
and `fable` is its alias; they are returned as one model, not as unsupported
guessed models.
Custom IDs never claim `available` without live evidence; configured values
are marked `origin:"custom"`/`availability:"unknown"` until verified.

## 9. `models` catalog and picker

`rolemux models [--refresh] [--runner codex|claude|copilot] [--json]` is
live-first and returns an array of records with this contract:

```json
{
  "id":"gpt-5-codex",
  "label":"GPT-5 Codex",
  "provider":"codex",
  "origin":"live",
  "availability":"available",
  "age_seconds":0,
  "efforts":["low","medium","high"],
  "default_effort":"medium",
  "aliases":["codex"],
  "custom":false
}
```

Catalog discovery is live-first for every runner. Successful results are
cached atomically with a timestamp and a hashed cache identity consisting of
provider, CLI/SDK-reported account identity, and non-secret gateway endpoint.
The raw identity is not written. No token, API key, or environment dump enters
the cache. The last good cache is retained when live discovery fails and is
marked `origin:"cache"`, `availability:"unknown"`, and includes an explicit
`age_seconds` value; absence of both live data and a cache is an actionable
discovery error. `--refresh` bypasses the cache for this call but still keeps
the last good cache if refresh fails. A failed live call never claims
`available`; its returned availability is `unknown`.

Codex discovery initializes the CLI connection and paginates `model/list` until
the server says there is no next cursor. Custom provider IDs from the safe
Codex routing extraction are included. Copilot discovery uses SDK `ListModels`
only when the pinned SDK is available and isolated. Claude has no paid-request
live model endpoint in v1: it uses no-paid local version/capability checks,
known aliases (`sonnet`, `opus`, `haiku`, canonical `claude-fable-5` with
alias `fable`), custom
configured IDs, and the last good cache. A Claude custom fallback is labeled
`origin:"custom"` and never claims live availability without evidence. Effort
support/default comes from live metadata where present; otherwise it is
unknown, not invented. Unknown availability is visible in both human and JSON
output so a picker can warn before selection.

The terminal picker filters by case-insensitive substring as the user types,
supports Up/Down, Enter, and a lone Escape. Escape is handled without a
blocking read and cancels cleanly. Selecting a model not present in live or
cached availability requires an explicit warning/confirmation. Effort options
come from the selected model's supported efforts/default when available;
unknown models offer a clearly labeled optional effort choice rather than
silently assigning one. Configure persists model and optional effort.

## 10. JSON contract and exit codes

Without `--json`, human-readable status goes to stdout and progress/diagnostics
to stderr. With `--json`, stdout contains exactly one JSON object, with no
progress, spinner, or incidental provider output. A successful response has:

```json
{
  "ok": true,
  "command": "code-review",
  "task": {"id":"...","phase":"approved","round":0},
  "result": {"status":"approved"},
  "advisories": []
}
```

Errors use the same single-object contract and stable machine code:

```json
{
  "ok": false,
  "error": {
    "code": "REVIEW_NEEDED",
    "message": "scoped files changed during code review",
    "retryable": true,
    "task_id": "..."
  }
}
```

Exit codes:

```text
0 success
2 usage/configuration/task-state error
3 needs_input; caller must provide plan/implementation answer
4 REVIEW_NEEDED; retryable scoped barrier mutation
5 provider/orchestrator action required, unsupported/fail-closed provider, or
  stale CAS completion
6 task operation already in flight
7 five-round plan or code-review exhaustion
```

Error classification is performed before generic wrapping using `errors.Is` and
structured provider error codes; JSON error codes and exit codes cannot drift.
There is no separate exit code for stale CAS: it remains a structured
`STALE_OPERATION` action error mapped to 5 and cannot steal the review-needed,
lock, or exhaustion codes. Needs-input responses are successful state writes
but return 3 so a host can supply the required answer.

## 11. Install and doctor

`rolemux install --global --hosts ...` installs the portable root `SKILL.md`
into these exact personal destinations (with platform home expansion):

```text
Claude Code: ~/.claude/skills/rolemux/SKILL.md
Codex:       ~/.codex/skills/rolemux/SKILL.md
Copilot:     ~/.copilot/skills/rolemux/SKILL.md
```

It creates parent directories with private permissions, writes a temporary
file in the destination directory, fsyncs, and renames atomically. If an
existing file is byte-identical, installation succeeds as an idempotent
no-op. If it differs, installation refuses with a conflict unless `--force`
is explicitly supplied; `--force` replaces only that destination atomically,
preserving the destination mode when possible. It never truncates, recursively
deletes, or replaces another path. Root `SKILL.md` and the embedded asset must
remain byte-identical. `agents/openai.yaml` has a `default_prompt` explicitly
mentioning `$rolemux`.

`doctor` reports only pass/fail and safe paths/versions. It checks:

- provider executable presence, version, and invocation shape;
- provider auth availability without printing credentials;
- sanitized permission/bypass environment state;
- installed skill content and embedded/root hash;
- PATH lookup and executable permissions;
- writable private state/cache directories and Git repository discovery.

## 12. Regression tests

All provider-facing tests inject fakes or deterministic subprocess fixtures.
Required tests include:

- config layer precedence, environment path handling, and secret redaction;
- hierarchical command parsing (`models`, `configure --global|--project`,
  `plan start|answer|review`, `implement answer`, and `code review`), exact
  project/global paths, one-profile `--role` updates using common
  `--runner/--model/--effort` flags, `--from` full-fragment import,
  before-write hash-drift conflict only, atomic plan-file writes, and exact
  exit mapping;
- canonical scope validation, symlink/traversal rejection, wildcard matching,
  scope overlap, default `.rolemux/**` exclusion, and exact HEAD-independent
  worktree/index manifest hashing for additions, deletions, symlinks,
  submodules/directories, staged/unstaged content, renames, and mode-only
  edits; reconstructible baseline blobs after HEAD movement; directory
  child-list hashes restricted to in-scope children; immutable content-
  addressed baseline refs and separate non-overwriting candidate refs;
- ownership-safe advisory lock behavior, duplicate same-task rejection,
  operation-token/CAS stale completion rejection, atomic writes, and
  `golang.org/x/sys/unix.Flock` ownership behavior;
- barrier test with two disjoint task IDs simultaneously inside long provider
  calls in the same checkout;
- reviewer scoped pre/post mutation returns exit 4, consumes no round, and
  `retry` resumes the same reviewer session;
- unrelated out-of-scope edits and unrelated HEAD advancement do not stale a
  scoped approval; overlap/unmatched/out-of-scope diagnostics are advisory;
- plan review checks only plan hash and is not staled by unrelated worktree
  activity; first implement accepts initially dirty scoped content;
- known-session provider failures (including the initial planner) preserve
  operation/prompt/findings/scope and retry; unknown-session failure remains
  fail-closed;
- profile snapshot immutability for all four runner/model/effort profiles and
  max-five-round separate plan/code counters; `planned -> needs_input`,
  planner needs-input/repeated answers, interrupted-loop persistence, and
  same-session plan findings loop; same-session implementer/code findings loop;
  strict provider-native JSON schemas (and Copilot exact-one-value fallback)
  reject prose inference; both fifth-round exhaustions return 7;
- Codex fresh `thread.started` requirement, concurrent stderr draining,
  kill-before-wait bounded-output failure, start/resume argument placement,
  effort/model fidelity, pagination, exact global `-C/-s/-a/--search` placement
  with only those flags before `exec`, ignore flags after `exec`, resume flags
  before `SESSION`, final stdin `-`, capability probing, and safe Codex TOML
  extraction/deterministic serialization without secrets;
- Claude exact case-sensitive tools, restricted/preapproval flags, strict empty
  MCP config, preassigned fresh-vs-resume session IDs, stdin prompt/schema,
  Cmd.Dir, resume isolation, outer-wrapper/session verification, and nested
  `structured_output` envelope decoding;
- Copilot path resolution, read-path confinement/symlink escape rejection, and
  fail-closed unsupported SDK behavior, exact-one-JSON fallback, and fresh/
  resume Provider/BYOK re-provisioning;
- picker filtering, arrow navigation, nonblocking Escape, effort choices, and
  unknown-availability warning;
- JSON one-value output and every exit/error classification, including
  needs-input 3, review-needed 4, stale CAS 5, in-flight 6, and exhaustion 7;
- conflict-safe installer destinations/idempotence/`--force` behavior and
  expanded doctor checks/version minimums; cache live failure returns unknown
  availability with origin/age and preserves last good data; Claude aliases
  include fable/claude-fable-5; custom TOML model fields validate per provider.

## 13. Verification and delivery constraints

Pinned build/runtime minimums are Go `1.24`, Codex CLI `0.153.3` with the
validated global/resume flag contract, Claude Code `2.1.260` with the listed
restricted flags, and Copilot Go SDK
`github.com/github/copilot-sdk/go v1.0.13-preview.4`, plus
`golang.org/x/sys/unix` pinned to the version selected in `go.mod`. A newer
provider must pass capability probes before use. The implementation must
report a precise unsupported-version error rather than guessing flag spellings
or relaxing permissions. Task locking uses and tests `unix.Flock` directly.

From `/Users/basant/workspace/rolemux`, run:

```text
gofmt -w $(rg --files -g '*.go')
GOCACHE=/private/tmp/rolemux-build-cache go vet ./...
GOCACHE=/private/tmp/rolemux-build-cache go test ./...
GOCACHE=/private/tmp/rolemux-build-cache go test -race ./...
GOCACHE=/private/tmp/rolemux-build-cache go build -o /private/tmp/rolemux-bin ./cmd/rolemux
```

Build output and cache remain outside the repository. Do not add a license and
do not commit or push from RoleMux or its workers. Any provider SDK/API
limitation is documented precisely and implemented fail-closed; security
permissions are never weakened to make a provider appear available.

Delivery authorization is already granted to the host/orchestrator, but remains
outside the RoleMux CLI and workers: after verification, inspect the direct
repository diff, commit the approved source, create a public GitHub repository,
and push after checking `gh` authentication and a repository-name collision.
There is no second authorization gate. These operations must never be triggered
by a workflow provider call.
