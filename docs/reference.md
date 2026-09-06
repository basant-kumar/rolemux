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
rolemux plan graph <task-id> --json
```

The graph returns dependency-ordered waves, ready unit IDs, write scopes, and
self-contained execution packets. Planner results declare `complexity` as
`trivial`, `small`, `medium`, `large`, or `system`. Trivial plans have one unit,
small plans at most two, and medium plans at most six. Scope fields accept bare
repository paths/globs only; prose and annotations are rejected. The
orchestrator may start independent units from the same wave concurrently:

```bash
rolemux work start <task-id> <unit-id> --json
rolemux implement <work-task-id> --json
rolemux code review <work-task-id> --json
```

Re-run `plan graph` after approvals to find newly ready units. Once every unit
passes, start the one-time integration gate:

```bash
rolemux work integrate <task-id> --json
```

For a one-unit trivial or small plan, this returns approval without another
provider review because the unit's focused code review is already the complete
integration boundary.

This starts one logical broad integration-review gate over the combined
approved changes. It uses dedicated durable integration reviewer and fixer
sessions. Each integration review invocation performs one reviewer turn and,
when changes are requested, at most one fix before returning control; issue
another explicit review command to review the fixed result. The gate can span
multiple host invocations. `work integrate` always expects the parent plan task
ID and derives the integration ID internally, so invoke it again with the
parent ID for subsequent integration reviews. Preserve the returned derived
integration task ID only for `implement answer` and `retry`.

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
| `status` | Durable outcome such as `approved`, `revised`, `fixed`, `needs_input`, `review_needed`, `no_progress`, `failed`, `in_flight`, or `exhausted`. |
| `review_kind` | The independent counter: `plan`, `code`, or `integration`. |
| `review_round` | Accepted reviewer verdicts for this kind. |
| `max_rounds` | Snapshotted ceiling; `0` means unlimited. |
| `can_review` | Whether the host may issue the next review command. |
| `next_action` | The safe host continuation, such as `plan_review`, `code_review`, `work_integrate`, `plan_answer`, `implement_answer`, `retry`, `inspect`, `wait`, `advance`, or `stop`. |
| `question` | Optional question when `status` is `needs_input`. |
| `source` | Optional planner/implementer answer source for that question. |

For example, a completed code fix is returned compactly as:

```json
{"status":"fixed","review_kind":"code","review_round":1,"max_rounds":5,"can_review":true,"next_action":"code_review"}
```

The host decision table is:

| Status | Host decision |
|---|---|
| `approved` | Advance the graph; only an approved node unlocks dependencies. |
| `revised` / `fixed` | Issue the matching explicit review; keep dependencies locked. |
| `needs_input` | Ask `question`, then use `source` to choose the matching answer command. |
| `review_needed` | Run `rolemux retry <task-id> --json`. |
| `no_progress` | Inspect the saved candidate and decide deliberately; do not repeat automatically. |
| `failed` / `in_flight` | Follow recovery metadata: retry a durable failure or wait/inspect an in-flight owner. |
| `exhausted` | Stop; the review ceiling has been reached. |

Hosts must preserve exact task IDs, schedule only graph-ready units, and never
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
rolemux list --json
```

`status` is compact unless `--full` is supplied. Ctrl+C, SIGTERM, and provider
timeouts save a resumable retry when a durable session exists. RoleMux fails
closed instead of replaying a turn in a fresh conversation.

## Usage accounting

`status` and `usage` persist the same per-role usage fields. `requests` is one
host-measured count per adapter invocation, and `prompt_bytes` is the byte
length of the actual request prompt after any initial capability note is
inserted. Provider response values cannot inflate either host counter.

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

- Plans: `.rolemux/plans/<task-id>.md`
- Private task/session state: `.git/rolemux`
- Global settings: `~/.rolemux/config.toml`
- Project override: `.rolemux.toml`

The entire `.rolemux/` directory is ignored. RoleMux uses short task-state locks,
not a repository-wide lock, so independent scoped units can share one checkout.
It rejects dependency graphs that schedule overlapping write scopes in parallel.
Workers do not commit, push, stash, reset, rebase, merge, or create worktrees.

## Skills and host handoffs

On each role's first turn, RoleMux sends a bounded inventory of available skill
names, descriptions, tools, provider scope, and installed helpers. It does not
copy skill bodies, credentials, environment values, or the inventory again on
resumed turns.

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
dashboard and event-log paths. No
separately managed daemon is required. Authentication and model discovery still
go directly to the provider.

If pxpipe is missing or cannot start, RoleMux reports or suggests installation
once and safely uses the provider directly. Model eligibility and actual savings
come from pxpipe itself; verify them with:

```bash
pxpipe stats --file <events-file>
```

RoleMux does not claim compression when no matching event exists and never
changes the selected model to enable pxpipe.

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
| `3` | A planner or implementer needs an answer |
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
