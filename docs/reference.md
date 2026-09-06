# RoleMux reference

This document covers the lower-level commands and behavior that host-agent
skills use to drive RoleMux. Most users only need the setup and prompt shown in
the main README.

## Configuration

Open the full-screen picker:

```bash
rolemux configure --global
```

The first screen selects all roles or one of planner, implementer, shared
reviewer, plan reviewer, and code reviewer. Escape returns to the previous
screen and Ctrl+C cancels. Model, effort, context, and speed choices appear only
when the provider advertises them. Contextual screens keep a highlighted active
role badge, current step, and relevant provider/model selections visible.

Profiles can also be updated atomically:

```bash
rolemux configure --global --role planner \
  --runner codex --model gpt-5.6-sol --effort max --speed priority

rolemux configure --global --role implementer \
  --runner codex --model gpt-5.6-luna --effort max

rolemux configure --global --role reviewer \
  --runner codex --model gpt-5.6-sol --effort xhigh
```

Review safety is configured by the top-level TOML key
`review_max_rounds`. Its default is five accepted reviewer verdicts per review
task:

```toml
review_max_rounds = 5
```

The CLI form is `rolemux configure --global --review-max-rounds N` (or
`--project --review-max-rounds N`), where `N` is any nonnegative integer,
including zero and positive custom limits:

```bash
rolemux configure --global --review-max-rounds 0   # remove the ceiling
rolemux configure --project --review-max-rounds 12 # positive custom limit
```

In the interactive configure UI, choose the `Review safety limit` entry. It
offers `Current`, `Default (5)`, `10`, and `Unlimited`; `Unlimited` writes
`review_max_rounds = 0`. The setting is top-level, not part of a role profile.

Use `--project` for a `.rolemux.toml` override in the current repository.
Configuration file selection is:

1. When set, `ROLEMUX_CONFIG=/absolute/path/config.toml` is the replacement
   configuration: it takes precedence for both reads and writes, and does not
   overlay normal project/global discovery.
2. When `ROLEMUX_CONFIG` is unset, project `.rolemux.toml` overlays global
   `~/.rolemux/config.toml`.

`review_max_rounds` follows this selection. An effective limit is snapshotted
when a task is created.
Work units and the derived integration task inherit their parent's snapshot;
later configuration edits do not change existing tasks. Historical tasks retain
a positive saved `max_rounds`; older task state without a positive saved value
defaults to five. A newly created task with an explicit zero keeps its unlimited
policy snapshot.

Example:

```toml
provider_turn_timeout_seconds = 900
review_max_rounds = 5

[budgets.planner]
max_turns = 20
max_tool_calls = 20
timeout_seconds = 300
max_output_bytes = 8388608

[budgets.implementer]
max_turns = 20
max_tool_calls = 12
timeout_seconds = 300
max_output_bytes = 8388608

[budgets.reviewer]
max_turns = 20
max_tool_calls = 3
timeout_seconds = 180
max_output_bytes = 4194304

[profiles.planner]
provider = "codex"
model = "gpt-5.6-sol"
effort = "max"
speed = "priority"

[profiles.implementer]
provider = "codex"
model = "gpt-5.6-luna"
effort = "max"

[profiles.reviewer]
provider = "codex"
model = "gpt-5.6-sol"
effort = "xhigh"
```

Budgets are task-lifetime model turns plus per-invocation tool calls, wall-clock
seconds, and process-output bytes. The shared `reviewer` budget feeds plan and
code review unless `[budgets.plan_reviewer]` or `[budgets.code_reviewer]`
overrides it. Positive values in global/project layers override the defaults;
new tasks snapshot the effective values.

For example, a project override can remove the ceiling or choose another
positive value:

```toml
# .rolemux.toml
review_max_rounds = 0
```

```toml
# .rolemux.toml
review_max_rounds = 12
```

Credentials are never stored in RoleMux configuration or task state.

## Model discovery

```bash
rolemux models
rolemux models --runner claude
rolemux models --runner codex --refresh
rolemux models --runner copilot --refresh --json
rolemux models --runner antigravity --refresh --json
```

Catalog entries identify live, cached, or custom origin and report availability,
effort, speed, descriptions, and context limits when the provider exposes them.
The picker renders cached providers immediately and refreshes only the selected
provider in the background.

## Workflow API

For a trivial local change whose task and exact write scope are already known,
create an implementation-ready task without planner, plan-reviewer, model-list,
or authentication probes:

```bash
rolemux quick start --task "Fix the active-role label" \
  --scope 'internal/cli/configure_view.go,internal/cli/configure_view_test.go' --json
rolemux implement <task-id> --json
rolemux code review <task-id> --json
```

The scope is mandatory and may not be `**`. The task is marked
`direct_implementation: true` with `complexity: trivial`.

Start, review, and inspect a plan:

```bash
rolemux plan start --task "Add resumable uploads" --json
rolemux plan review <task-id> --json
rolemux approval show <task-id>
rolemux approval respond <task-id> --gate <gate-id> --decision approve --human-confirmed
rolemux plan graph <task-id> --json
```

The default JSON graph is a compact scheduling view: dependency waves, ready
unit IDs, write scopes, context groups, estimates, and the critical path. Use
`--full --json` only when complete execution packets, context files, affected
symbols, criteria, and validation commands are needed. Planner results declare `complexity` as
`trivial`, `small`, `medium`, `large`, or `system`. Trivial plans have one unit,
small plans at most two, and medium plans at most six. Scope fields accept bare
repository paths/globs only; prose and annotations are rejected. The
orchestrator may start independent units from the same wave concurrently:

```bash
rolemux work start <task-id> <unit-id> --json
rolemux implement <work-task-id> --json
rolemux code review <work-task-id> --json
```

Dependency-ordered units with the same `context_group` reuse the prior
implementer session. Parallel units must have distinct context groups, avoiding
repeated repository discovery while preserving safe concurrency. Re-run `plan
graph` after approvals to find newly ready units. Once every unit passes, start
the one-time integration gate:

```bash
rolemux work integrate <task-id> --json
```

For a one-unit trivial or small plan, this creates the final human gate without
another provider review because the unit's focused code review is already the
complete integration boundary.

This starts one logical broad integration-review gate over the combined
approved changes. It uses dedicated durable integration reviewer and fixer
sessions. Each integration review invocation performs one reviewer turn and,
when changes are requested, at most one fix before returning control; issue
another explicit review command to review the fixed result. The gate can span
multiple host invocations. `work integrate` always expects the parent plan task
ID and derives the integration ID internally, so invoke it again with the
parent ID for subsequent integration reviews. Preserve the returned derived
integration task ID only for `implement answer` and `retry`.

## Human approval gates

Plan-reviewer approval pauses before execution. Final code-reviewer approval
pauses before completion. Inspect the immutable report and respond using its
exact gate ID:

```bash
rolemux approval show <task-id> [--json]
rolemux approval respond <task-id> --gate <gate-id> --decision approve --human-confirmed [--json]
rolemux approval respond <task-id> --gate <gate-id> \
  --decision request_changes --feedback "Keep the existing API" --human-confirmed [--json]
rolemux approval respond <task-id> --gate <gate-id> --decision discuss --human-confirmed [--json]
```

Approve and Discuss are provider-free. Discuss leaves the gate pending.
Request changes resumes the saved planner, direct implementer, or dedicated
integration fixer; the changed result must be reviewed again and produces a
new gate. Repeating the same decision for the same gate is idempotent, while a
stale or conflicting gate ID fails closed. Parent plan IDs resolve an existing
integration gate through `approval_task_id`.

For final code approval, `approval show` also prints path-limited local `git
status` and `git diff` commands. If the repository has a GitHub remote and the
GitHub CLI is installed and authenticated, the human can opt into a temporary
draft-PR review:

```bash
rolemux approval publish <task-id> [--json]
# Add review comments on the draft PR.
rolemux approval sync <task-id> [--json]
```

`publish` is the explicit authorization to push two `rolemux-review/*`
branches and create or update a draft PR. It uses the existing `git` and `gh`
CLIs and stores no credentials. The commits are built from RoleMux's immutable
baseline and reviewed-candidate snapshots in an isolated temporary worktree,
so the user's current branch, index, and worktree are unchanged. The draft is a
review surface only and must not be merged.

`sync` imports only comments and reviews newer than its saved cursors. Any
non-empty new feedback becomes `request_changes` and resumes the same saved
implementer (or integration fixer) session. After the fix and another model
review, `publish` updates the existing candidate branch and reuses the same PR.
The final RoleMux `approval respond ... --decision approve` remains the
authoritative completion action; a GitHub approval does not bypass that hard
gate. If GitHub, `gh`, login, or a supported remote is unavailable, use the
printed local review commands and `approval respond` instead.

## Host-controlled review continuation

Every `plan review`, `code review`, or integration review command performs one
reviewer turn and at most one requested revision or fix. It returns control to
the host after that operation, whether the result is approved, revised/fixed,
needs input, no progress, or a recoverable failure. A `revised` or `fixed`
result is not approval: use its `next_action` for another explicit review.
`plan answer`, `implement answer`, and `retry` likewise return after their
completed operation and never trigger an automatic re-review.

Review rounds are counted separately for the plan and for each code or
integration task. The counter advances only for an accepted reviewer verdict;
provider failures and stale-candidate retries do not consume a round. Approval
at the ceiling succeeds. A rejection at the ceiling returns `exhausted`
without starting another fix. `review_max_rounds = 0` removes the ceiling, but
does not remove the one-reviewer-turn/one-fix boundary for a single invocation.

Compact review control results contain these fields:

| Field | Meaning |
|---|---|
| `status` | Durable outcome such as `approval_required`, `approved`, `revised`, `fixed`, `needs_input`, `review_needed`, `no_progress`, `failed`, `in_flight`, or `exhausted`. |
| `review_kind` | The independent counter: `plan`, `code`, or `integration`. |
| `review_round` | Accepted reviewer verdicts for this kind. |
| `max_rounds` | Snapshotted ceiling; `0` means unlimited. |
| `can_review` | Whether the host may issue the next review command. |
| `next_action` | The safe host continuation, such as `approval_respond`, `plan_review`, `code_review`, `work_integrate`, `plan_answer`, `implement_answer`, `retry`, `budget_extend`, `inspect`, `wait`, `advance`, or `stop`. |
| `question` | Approval prompt or planner/implementer question. |
| `source` | Optional planner/implementer answer source for that question. |
| `events` | Bounded lifecycle facts such as review started, changes requested, fix started/completed, and approval, including structured findings. |
| `progress` | Current role/operation with model-turn and tool-call counters; provider prose and raw tool output are excluded. |

Pending human gates also expose `approval_id`, `approval_task_id`,
`approval_kind`, ordered `choices`, `artifact_path`, `scope`, and compact
`changed_files`.

For example, a completed code fix is returned compactly as:

```json
{"status":"fixed","review_kind":"code","review_round":1,"max_rounds":5,"can_review":true,"next_action":"code_review"}
```

The host decision table is:

| Status | Host decision |
|---|---|
| `approval_required` | Show the artifact, question, and choices; wait for the human. Never approve automatically. |
| `approved` | Advance the graph; only an approved unit unlocks dependencies. |
| `revised` / `fixed` | Issue the matching explicit review; keep dependencies locked. |
| `needs_input` | Ask `question`, then use `source` to choose the matching answer command. |
| `review_needed` | Run `rolemux retry <task-id> --json`. |
| `budget_exhausted` | Inspect partial work, extend only the named limit by the smallest practical increment, then retry the saved session. |
| `no_progress` | Inspect the saved candidate and decide deliberately; do not repeat automatically. |
| `failed` / `in_flight` | Follow recovery metadata: retry a durable failure or wait/inspect an in-flight owner. |
| `exhausted` | Stop; the review ceiling has been reached. |

`status: approved` is the review outcome; `next_action: advance` is the graph
transition. Human output presents them separately. Hosts must preserve exact
task IDs, schedule only graph-ready units, and never
unlock dependencies on `revised` or `fixed`. For integration, use the parent
plan ID for every `work integrate` review; reserve the returned derived ID for
answers and retries.

## Questions and recovery

Questions return to the host without discarding provider context:

```bash
rolemux plan answer <task-id> --answer "Use multipart uploads" --json
rolemux implement answer <task-id> --answer "Keep the existing API" --json
```

Use the returned `source` (or `pending_question_source` in task state) to pick
the matching answer command. An answer completes the current durable provider
operation and returns control; it does not automatically re-review the result.
The same host-owned rule applies to `retry`: inspect its result and issue the
next explicit review when `can_review` is true.

Operational commands:

```bash
rolemux status <task-id> --json
rolemux status <task-id> --full --json
rolemux usage <task-id> --json
rolemux retry <task-id> --json
rolemux budget show <task-id> --json
rolemux budget extend <task-id> --role implementer --tool-calls 3 --json
rolemux budget extend <task-id> --role implementer --output-bytes 1048576 --json
rolemux work adopt <task-id> --note "Host completed the scoped fallback" --json
rolemux list --json
```

An exhausted task cannot be retried until its named budget dimension is
extended. Recovery extensions cannot inflate unrelated dimensions, and one
extension cannot exceed that dimension's current limit; inspect the resumed
result before extending again.

`status` is compact unless `--full` is supplied. Ctrl+C, SIGTERM, and provider
timeouts save a resumable retry when a durable session exists. RoleMux fails
closed instead of replaying a turn in a fresh conversation.
`list --json` is a compact task index; query one task with `status` or `usage`
instead of loading every task's findings, profiles, and counters.

Budget extension and adoption are provider-free recovery actions. Extending a
budget never starts work; inspect the state and issue `retry`. Adoption is only
allowed at an interrupted implementation boundary with a previously captured
scope/baseline and a nonblank audit note. It captures the current scoped delta
for ordinary review; it cannot replace approved or approval-pending code.

## Usage accounting

`status` and `usage` persist the same per-role usage fields. `requests` is the
backward-compatible JSON name for host-measured provider invocations.
`agent_turns` counts observed model turns (`codex` is a conservative estimate
because its CLI does not expose the internal count), `tool_calls` counts
observed tool starts, and `prompt_bytes` is the actual request size after any
initial capability note is inserted. Provider response values cannot inflate
host-owned invocation or prompt counters.

Token fields (`input_tokens`, `cached_input_tokens`, `cache_write_tokens`,
`output_tokens`, `reasoning_tokens`, and `total_tokens`) are retained exactly
as reported. `unreported_requests` counts invocations with no token snapshot;
`incomplete_requests` counts partial snapshots, including snapshots retained
when cancellation, timeout, or another invocation error interrupts a turn.
These counters are additive: a later same-session retry adds its result and
does not erase earlier uncertainty.

Adapters that do not set `usage_status` are interpreted conservatively. A
nonzero token counter is treated as reported, and an invocation error makes
that legacy snapshot incomplete. A response with no token counter is
unreported; request counts, prompt bytes, and existing aggregate metadata do
not establish token-report presence. Cumulative provider snapshots are
delta-applied only when a snapshot is actually reported, so a missing report
does not reset the previous cumulative snapshot. Provider-specific cached-input
normalization remains unchanged; uncached input is derived per role before
mixed-provider totals are summed.

Human output says when tokens are unreported and labels mixed or partial data
as incomplete reported totals. JSON remains one envelope with the existing
field names plus the additive usage counters.

## State and concurrency

- Plans, approval reports, and task/session state: private Git state resolved
  from `git rev-parse --git-path rolemux` (normally `.git/rolemux`)
- Global settings: `~/.rolemux/config.toml`
- Project override: `.rolemux.toml`

RoleMux does not create or require a project `.rolemux/` directory. It uses
short task-state locks, not a repository-wide lock, so independent scoped units
can share one checkout.
It rejects dependency graphs that schedule overlapping write scopes in parallel.
Workers do not commit, push, stash, reset, rebase, merge, or create worktrees.

## Skills and host handoffs

On each role's first turn, RoleMux sends a task-ranked, bounded inventory of
relevant skill names, descriptions, tools, provider scope, and installed
helpers. It does not copy skill bodies, credentials, environment values, or the
inventory again on resumed turns.

When a capability exists only in the host—such as a UI tool or connector—the
worker returns a precise question. The orchestrator gathers the evidence and
answers the same provider session.

Install or refresh host skills with:

```bash
rolemux install --global --hosts all
```

This covers Claude, shared `.agents` hosts such as Codex, Copilot, and
Antigravity. Existing different files require `--force`; identical files are an
idempotent success.

## pxpipe

For eligible Claude and Codex task turns, RoleMux can start a private foreground
[pxpipe](https://github.com/teamchong/pxpipe) process and prints its temporary
dashboard and event-log paths. No separately managed daemon is required.
Authentication and model discovery still go directly to the provider.

If pxpipe is missing or cannot start, RoleMux reports or suggests installation
once and safely uses the provider directly. Model eligibility and actual savings
come from pxpipe itself; verify them with:

```bash
pxpipe stats --file <events-file>
```

After each wrapped turn, stderr reports the actual pxpipe event mode, model,
compression flag, and savings when supplied. `mode=text` is explicitly labeled
pass-through; `mode=image` confirms image transport. RoleMux does not assume
Luna, Sol, Astra, or any other model is eligible, does not claim compression
without a matching event, and never changes the selected model to enable it.

## Provider boundaries

- Claude retains its native Skill tool but runs with restricted permissions and
  an empty MCP allowlist.
- Codex and Antigravity retain native skill discovery and sandboxes.
- Copilot SDK sessions disable ambient shell, MCP, hooks, memory, extensions,
  and subagents. Only implementers receive repository- and scope-confined edits.
- Provider/model/effort/speed mismatches fail closed with an actionable error.

## Exit codes

| Exit | Meaning |
|---:|---|
| `0` | Completed successfully |
| `2` | Usage, configuration, or task-state error |
| `3` | Human approval or a planner/implementer answer is required; inspect `status` and `next_action` |
| `4` | Scoped files changed during review; retry saved review |
| `5` | Provider/orchestrator action required, operation failed closed, or review reports `no_progress` |
| `6` | The task already has an operation in flight |
| `7` | The configured plan, code, or integration review limit was exhausted |

With `--json`, stdout contains one JSON object; progress and diagnostics go to
stderr. Exit `0` means the operation completed, not that a review approved.

## Adding a provider

Provider CLIs implement `runner.Adapter` and register once in the runner
registry. Catalog, picker, session persistence, workflow state, and command
handlers remain provider-neutral. Transport helpers use a separate
provider-scoped lifecycle interface so optional proxies cannot affect login or
model discovery.
