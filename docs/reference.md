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
when the provider advertises them.

Profiles can also be updated atomically:

```bash
rolemux configure --global --role planner \
  --runner codex --model gpt-5.6-sol --effort max --speed priority

rolemux configure --global --role implementer \
  --runner codex --model gpt-5.6-luna --effort max

rolemux configure --global --role reviewer \
  --runner codex --model gpt-5.6-sol --effort xhigh
```

Use `--project` for a `.rolemux.toml` override in the current repository.
Configuration precedence is:

1. `ROLEMUX_CONFIG=/absolute/path/config.toml`
2. project `.rolemux.toml`
3. global `~/.rolemux/config.toml`

Example:

```toml
provider_turn_timeout_seconds = 900

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

Start, review, and inspect a plan:

```bash
rolemux plan start --task "Add resumable uploads" --json
rolemux plan review <task-id> --json
rolemux plan graph <task-id> --json
```

The graph returns dependency-ordered waves, ready unit IDs, write scopes, and
self-contained execution packets. The orchestrator may start independent units
from the same wave concurrently:

```bash
rolemux work start <task-id> <unit-id> --json
rolemux implement <work-task-id> --json
rolemux code review <work-task-id> --json
```

Re-run `plan graph` after approvals to find newly ready units. Once every unit
passes, run the one-time integration gate:

```bash
rolemux work integrate <task-id> --json
```

Integration findings go to one fresh integration implementer. RoleMux then
resumes that fixer and the same integration reviewer until approval or the
five-round limit.

## Questions and recovery

Questions return to the host without discarding provider context:

```bash
rolemux plan answer <task-id> --answer "Use multipart uploads" --json
rolemux implement answer <task-id> --answer "Keep the existing API" --json
```

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
pxpipe process and prints its temporary dashboard and event-log paths. No
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
| `5` | Provider/orchestrator action required or operation failed closed |
| `6` | The task already has an operation in flight |
| `7` | Five review rounds were exhausted |

With `--json`, stdout contains one JSON object; progress and diagnostics go to
stderr.

## Adding a provider

Provider CLIs implement `runner.Adapter` and register once in the runner
registry. Catalog, picker, session persistence, workflow state, and command
handlers remain provider-neutral. Transport helpers use a separate
provider-scoped lifecycle interface so optional proxies cannot affect login or
model discovery.
