---
name: rolemux
description: Orchestrate planning, implementation, and review through RoleMux when a task should use configured models while preserving provider sessions and host-controlled review continuation.
---

# RoleMux

Use the `rolemux` binary as the workflow driver while remaining the user-facing
orchestrator. RoleMux selects the configured provider/model for each role and
persists the exact provider session used by that role.

## Run a task

1. Classify the request before spending provider turns. For an obvious local
   change with a self-contained task and exact narrow write scope, run
   `rolemux quick start --task "<task>" --scope '<paths>' --json`, retain its
   task ID, then go directly to `rolemux implement` and `rolemux code review`.
   This path intentionally makes no planner, plan-reviewer, model-list, or auth
   probe call. Never use it with `**` or when architecture/research is needed.
2. Run `rolemux doctor --json` when provider readiness is unknown. If it reports
   a missing CLI or login, return that action to the user; do not silently choose
   another model.
3. For non-trivial work, start planning with `rolemux plan start --task "<task>" --json`. Retain the
   returned task ID for every later command.
4. When a command exits `3`, inspect `status`. For `approval_required`, show the
   exact question, immutable artifact path, scope/changed files, gate ID, and
   all choices (Approve, Request changes, and Discuss), then wait for an
   explicit human decision. Use `rolemux approval respond` with the exact task
   and gate IDs, one of `approve`, `request_changes`, or `discuss`, and feedback
   when requesting changes. Never approve automatically. For
   `needs_input`, present the question and resume with `rolemux plan answer
   <task-id> --answer "<answer>" --json` or `rolemux implement answer <task-id>
   --answer "<answer>" --json`, according to `pending_question_source`.
5. Run `rolemux plan review <task-id> --json`. Each review command performs one
   planner-reviewer turn and, if changes are requested, at most one planner
   revision, then returns control. A `revised` result requires another explicit
   `rolemux plan review` to review the new plan. The command reuses the durable
   planner and reviewer sessions. Reviewer approval creates a human plan gate;
   do not start execution until that exact gate is approved. `plan answer`,
   `implement answer`, and
   `retry` also return after their completed operation without automatically
   starting another review.
6. Read `rolemux plan graph <task-id> --json`. The graph exposes semantic
   `complexity`: trivial plans have one unit, small at most two, and medium at
   most six. RoleMux rejects prose-like scope entries and graphs that exceed
   their declared class. For every ID in `ready`, run
   `rolemux work start <task-id> <unit-id> --json`, then use the returned work
   task ID with `rolemux implement <work-task-id> --json` and one
   `rolemux code review <work-task-id> --json` command at a time. A code review
   command performs one reviewer turn and, if needed, at most one implementer
   fix, then returns control; explicitly review a `fixed` result again. Same-
   wave units may run concurrently, but schedule only graph-ready units and do
   not unlock dependencies on `revised` or `fixed`. Preserve every exact task
   ID and re-read the compact graph after approvals.
7. After every graph node is approved, run `rolemux work integrate <task-id>
   --json`. This is one logical broad integration-review gate over the combined
   approved changes. It creates or resumes dedicated durable integration
   reviewer and fixer sessions; each invocation performs one reviewer turn and,
   if needed, at most one fix before returning control. Continue it with the
   explicit command named by the result, potentially across multiple host
   invocations. `work integrate` expects the parent plan task ID on every
   integration review; invoke it again with the original `<task-id>`. Use the
   derived integration task ID returned by `work integrate` only for
   `implement answer` and `retry`. A one-unit trivial/small graph returns
   final human approval gate without another provider call because its focused
   review is already the complete reviewer boundary. Completion still requires
   the human to approve that gate.
8. Inspect `rolemux status <task-id> --json` whenever recovery is needed. Use
   `rolemux retry <task-id> --json` when status reports a retryable saved
   operation, or when an in-flight operation's owning RoleMux process was
   terminated. RoleMux resumes only a durable provider session; it will not
   duplicate an abandoned turn in a fresh session.

The orchestrator may run graph nodes listed as ready concurrently in the same
checkout. The planner graph supplies exact scopes and RoleMux rejects unsafe
parallel overlap; dependency edges serialize overlapping work.

Delegate task source edits and review-fix edits to the RoleMux implementer; do
not bypass the configured implementation role by patching those scoped files as
the host orchestrator. The orchestrator may inspect state, coordinate disjoint
tasks, and run authorized build or Git commands outside the provider workflow.

Keep orchestration token-conscious: pass task IDs instead of replaying earlier
provider output, rely on RoleMux's resumed sessions, and avoid calling `status`
or refreshing `models` unless the workflow or recovery actually requires it.
Never omit new user constraints or evidence needed for correctness.

The implementer receives an explicit pre-edit discovery budget: at most three
batched read/search calls over named files and symbols, with no repository-wide
search or Git history/status/diff before editing. If the execution packet is not
sufficient inside that budget, return `needs_input` instead of researching
outward.

RoleMux gives each fresh delegated role a bounded inventory of provider-native
skills, provider tools, installed helpers, and skills that may exist only in a
different host. A delegated role must not invoke the `rolemux` skill recursively.
When a `needs_input` question requests a host-mediated capability, first use an
applicable skill, connector, or read-only external tool available in this
orchestrator CLI, then pass only the concise relevant evidence through the
matching `plan answer` or `implement answer` command. Ask the user instead when
the request needs a decision, credentials, new authority, or a mutating external
action. Never copy whole skill bodies, credentials, or unbounded tool output.

When installed, pxpipe may wrap Claude task turns and may wrap Codex task turns
only after RoleMux positively verifies ChatGPT authentication and the supported
route. RoleMux owns a private server per eligible turn; no user-managed daemon
is required. The temporary dashboard URL and persistent event path are printed
to stderr. Treat wrapping as transport only: `pxpipe stats --file
<events-file>` is the authority on measured savings, and a model that pxpipe
does not enable passes through unchanged. A missing/incompatible helper or an
API-key/unknown Codex route falls back to the original direct command. When the
helper is missing, RoleMux suggests its install command once on the first turn
of each new eligible provider session and does not repeat it on resumes.

Use `rolemux usage <task-id> --json` for a compact per-role token comparison.
`rolemux status <task-id> --json` is compact by default; use `--full` only for
deep diagnostics and never feed full state back into a model unnecessarily.

## Review limits and host continuation

The top-level TOML setting `review_max_rounds = 5` is the default safety limit.
The CLI form is `rolemux configure --global --review-max-rounds N` (or
`--project`), where `N` is any nonnegative integer. Use
`rolemux configure --global --review-max-rounds 0` for an unlimited ceiling
or `rolemux configure --project --review-max-rounds 12` for a positive custom
limit. The CLI accepts any nonnegative integer. Interactive configure has a
`Review safety limit` entry with `Current`, `Default (5)`, `10`, and `Unlimited`
choices. When `ROLEMUX_CONFIG` is set, RoleMux uses that file alone and replaces
normal project/global discovery. When it is unset, project `.rolemux.toml`
overlays global `~/.rolemux/config.toml`; `review_max_rounds` follows this
selection.

RoleMux snapshots the effective limit into each new task. Work units and the
derived integration task inherit the parent snapshot, so later configuration
edits do not change an existing task. Historical tasks retain a positive saved
`max_rounds`; older task state without a positive saved value defaults to five.
The limit counts accepted reviewer verdicts separately for the plan and for
each code or integration task. Provider failures and stale-candidate retries do
not consume a round. Approval at the ceiling succeeds; rejection at the ceiling
returns `exhausted` without starting another fix. `0` removes the ceiling, not
the single-invocation boundary.

Compact review control results contain `status`, `review_kind`, `review_round`,
`max_rounds`, `can_review`, and `next_action`, with optional `question` and
`source` for host-mediated questions. For example, after one code fix:

```json
{"status":"fixed","review_kind":"code","review_round":1,"max_rounds":5,"can_review":true,"next_action":"code_review"}
```

Use the result as the host decision:

| Status | Host action |
|---|---|
| `approval_required` | Show the report and choices, then wait for the human; never infer or issue approval. |
| `approved` | Advance the graph; only an approved work-unit node unlocks dependencies. |
| `revised` / `fixed` | Issue the matching explicit review; keep dependencies locked. |
| `needs_input` | Ask `question`, then use `source` with the matching `plan answer` or `implement answer` command. |
| `review_needed` | Run `rolemux retry` using the exact task ID. |
| `no_progress` | Inspect the saved candidate and return a deliberate host decision; do not repeat automatically. |
| `failed` / `in_flight` | Follow recovery metadata: retry a durable failure or wait/inspect an in-flight owner. |
| `exhausted` | Stop; the configured review ceiling has been reached. |

Treat `status` as the outcome and `next_action` as the transition. Present them
separately to humans (for example, `Review verdict: Approved` and `Next step:
Start mobile-layout`), never as one ambiguous phrase. Treat JSON stdout as the
sole machine-readable result; diagnostics are on stderr. Exit `0` means the
operation completed, not that it was approved. Exit
`2` is a usage, configuration, or task-state error; exit `3` requires human
approval or a user answer, distinguished by `status`; exit `4` means scoped
files changed during review and the saved review should be retried; exit `5`
requires provider/orchestrator action, includes a
failed-closed operation, or reports `no_progress`; exit `6` means the same task
already has an operation in flight; exit `7` means the configured plan, code,
or integration review limit was exhausted. Never infer approval from prose or
continue past a failed review gate.
RoleMux workers do not commit, push, stash, reset, rebase, or merge. Perform
repository and build commands separately, within the user's authorization.

Use `rolemux models --json` for agent-friendly discovery or `rolemux configure`
in a terminal for the searchable model picker. The picker reads its
account-scoped cache immediately and refreshes the selected provider in the
background; request `models --refresh` only when a blocking live result is
specifically required.
