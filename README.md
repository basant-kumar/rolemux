# RoleMux

**Right model. Right role. Same thread.**

RoleMux is a thin, host-friendly CLI that lets Claude Code, Codex, GitHub
Copilot CLI, or another orchestrator assign different models to planning,
implementation, and review. Each role keeps its provider session across
questions and review fixes, so context is resumed instead of reconstructed.

RoleMux does not replace the host agent. The host remains the orchestrator and
decides when to plan, implement, review, retry, or run independent tasks in
parallel.

## Token discipline

RoleMux treats tokens as a resource, without silently dropping context needed
for correctness. A role receives the complete task context on its first turn;
later turns resume that exact provider session and send only new answers,
review findings, revised artifacts, or scoped deltas. Prompts tell workers to
inspect only relevant files and to avoid restating inputs. Strict small JSON
envelopes, task scopes, bounded process output, and the five-round review cap
prevent accidental context growth.

RoleMux does not guess at token counts, truncate plans, or replace required
evidence with a summary. Provider/model token limits configured for gateways
are honored by their adapters. `rolemux usage <task-id> --json` reports
per-role provider requests, prompt bytes, and input/cache/output/reasoning/total
tokens whenever the selected provider exposes usage data.

## Status

RoleMux v1 is macOS-first and built as a single Go binary. It supports:

- separate planner, implementer, shared reviewer, plan-reviewer override, and
  code-reviewer override profiles;
- live-first, account-aware model discovery with an explicit cache/custom
  fallback and machine-readable JSON;
- a searchable, full-screen model, effort, and speed picker with live model
  descriptions and context limits when the provider exposes them;
- durable provider sessions and restart-safe questions/retries;
- automatic plan-review and code-review fix loops, capped at five rounds;
- multiple task IDs running concurrently in one shared checkout;
- global defaults with project-local TOML overrides;
- existing CLI authentication and explicitly configured gateways/BYOK;
- installable host skills for Claude Code, Codex, and GitHub Copilot CLI.

Copilot models can plan and review through the pinned Copilot SDK. Copilot is
fail-closed as an implementation provider in v1 until its write isolation and
resume surface can satisfy the same guarantees as the Codex and Claude
adapters. RoleMux never silently switches to another model or provider.

## Install on macOS

Go 1.24 or newer is required.

```bash
go install github.com/basant-kumar/rolemux/cmd/rolemux@latest
```

Go installs the binary in `GOBIN`, or in `GOPATH/bin` when `GOBIN` is unset.
Resolve that directory and add it to the current shell path, then install the
host-agent skill and verify the setup:

```bash
ROLEMUX_BIN_DIR="$(go env GOBIN)"
[ -n "$ROLEMUX_BIN_DIR" ] || ROLEMUX_BIN_DIR="$(go env GOPATH)/bin"
export PATH="$ROLEMUX_BIN_DIR:$PATH"

rolemux help
rolemux install --global --hosts all
rolemux doctor --json
rolemux configure --global
```

Add the same `export PATH=...` setting to `~/.zshrc` to make it persistent in
new terminals.

From an existing source checkout, install the current revision with:

```bash
go install ./cmd/rolemux
```

RoleMux uses installed provider CLIs and their existing logins. The configure
picker shows each provider's sign-in state. Selecting an installed provider
that needs authentication temporarily leaves the picker, runs its official
login (`claude auth login`, `codex login`, or `copilot login`), and returns to
a freshly discovered model list when login succeeds. A missing CLI stays in
the provider screen with installation guidance; on macOS, install GitHub
Copilot CLI with `brew install copilot-cli` and select it again.

## Configure roles

Open the searchable picker for global defaults:

```bash
rolemux configure --global
```

Each wizard step replaces the previous screen. The model screen shows the
provider-returned name, description, context window, maximum context window,
default marker, and speed-mode availability when those fields exist. Effort
and speed get their own screens and appear only when the selected model
advertises them. Use the arrow keys and Enter to select, type to search
provider/model lists, press Escape to go back, and press Ctrl+C to cancel from
any step.

Or update one profile atomically:

```bash
rolemux configure --global --role planner \
  --runner codex --model gpt-5.6-sol --effort max --speed priority

rolemux configure --global --role implementer \
  --runner codex --model gpt-5.6-luna --effort max

rolemux configure --global --role reviewer \
  --runner codex --model gpt-5.6-sol --effort xhigh
```

Use `--role plan-reviewer` or `--role code-reviewer` only when that review role
needs to differ from the shared reviewer profile. Use `--project` to write an
override to the current repository's `.rolemux.toml`. For non-interactive
configuration, pass the provider-native speed option ID reported by
`rolemux models --runner <provider> --refresh --json`; `standard` selects the
provider default.

Configuration paths:

- global: `${XDG_CONFIG_HOME:-$HOME/.config}/rolemux/config.toml`
- project: `<repository>/.rolemux.toml`
- explicit replacement: `ROLEMUX_CONFIG=/absolute/path/to/config.toml`

Example TOML:

```toml
[profiles.planner]
provider = "codex"
model = "gpt-5.6-sol"
effort = "max"
speed = "priority"

[profiles.reviewer]
provider = "codex"
model = "gpt-5.6-sol"
effort = "xhigh"

[profiles.implementer]
provider = "codex"
model = "gpt-5.6-luna"
effort = "max"
```

Credentials are not stored in RoleMux configuration or task state. Gateway
configuration stores only non-secret endpoints and environment-variable names;
credential values are reacquired for every fresh and resumed provider turn.

## Discover models

Inspect the human-readable catalog:

```bash
rolemux models
rolemux models --runner claude
rolemux models --runner copilot --refresh
```

Or consume the same catalog as JSON:

```bash
rolemux models --json
```

Catalog records distinguish `live`, `cache`, and `custom` origin and report
availability as `available`, `unavailable`, or `unknown`. Model IDs and
capabilities are not compiled into RoleMux: Codex comes from its app-server
catalog (with context limits enriched from its own model cache), Claude comes
from its stream-JSON initialization handshake, and Copilot comes from the
installed CLI's SDK model list. Cached results include their age. Unknown
availability is never presented as a successful live probe. `--json` includes
structured effort and speed options plus context limits when advertised.

## Run a workflow

```bash
rolemux plan start --task "Add resumable uploads" --json
rolemux plan review <task-id> --json
rolemux implement <task-id> --scope 'internal/upload/**,cmd/server/**' --json
rolemux code review <task-id> --json
```

The plan is written to `.rolemux/plans/<task-id>.md`. Private task/session state
lives under `git rev-parse --git-path rolemux`, normally `.git/rolemux`, so it
does not become a tracked project artifact.

When a planner or implementer asks a question, RoleMux returns it to the host:

```bash
rolemux plan answer <task-id> --answer "Use multipart uploads" --json
rolemux implement answer <task-id> --answer "Keep the existing API" --json
```

The answer resumes the exact same provider session. Review findings likewise go
back to the same planner or implementer session, then return to the same reviewer
session until approved or the five-round limit is reached.

Recovery commands:

```bash
rolemux status <task-id> --json
rolemux usage <task-id> --json
rolemux retry <task-id> --json
rolemux list --json
```

`rolemux usage` is the compact token comparison view. It reports measured
request counts and prompt bytes plus provider-reported input, cache, output,
reasoning, and total tokens for each delegated role. Host-orchestrator usage is
not available through provider CLIs and is therefore not estimated.

## Shared-checkout concurrency

RoleMux does not create worktrees or impose a repository-wide lock. The
orchestrator may run independent task IDs concurrently in the same checkout so
agents see one another's changes. Only short task-state updates are locked.

Scopes are advisory ownership boundaries and review boundaries. Overlaps,
unmatched patterns, and out-of-scope changes are reported as structured
warnings; they do not globally block another task. A code-review approval binds
only the task's scoped baseline-to-candidate content, so unrelated edits do not
invalidate it.

RoleMux and its workers never commit, push, stash, reset, rebase, merge, or
create worktrees. The host orchestrator remains responsible for build and Git
commands within the user's authorization.

## Agent-facing exit codes

| Exit | Meaning |
|---:|---|
| `0` | Command completed successfully |
| `2` | Usage, configuration, or task-state error |
| `3` | Planner/implementer needs a user answer |
| `4` | Scoped files changed during review; retry the saved review |
| `5` | Provider or orchestrator action required, or operation failed closed |
| `6` | The same task already has an operation in flight |
| `7` | Five plan-review or code-review rounds were exhausted |

With `--json`, stdout contains exactly one JSON object. Progress and diagnostics
go to stderr.

## Install the host skill

```bash
rolemux install --global --hosts all
```

This installs the bundled skill at:

- `~/.claude/skills/rolemux/SKILL.md`
- `~/.codex/skills/rolemux/SKILL.md`
- `~/.copilot/skills/rolemux/SKILL.md`

An identical existing file is an idempotent success. A different file is never
overwritten unless `--force` is explicitly supplied.

## Security model

- Provider model/effort/speed/routing choices are snapshotted when a task is created.
- Secrets are never written to task state, catalog caches, argv, or diagnostics.
- Codex and Claude runs use explicit non-interactive sandbox/tool restrictions.
- Copilot planning/review uses SDK empty mode with only repository-confined
  `view`/`grep` reads and `web_fetch`; shell, writes, MCP, hooks, memory,
  extensions, and subagents are denied.
- Provider/version capability mismatches fail closed with an actionable error.

Run `rolemux doctor --json` to inspect CLI versions, authentication, paths,
permissions, skill integrity, and writable runtime directories without exposing
credential values.

## Adding another provider CLI

Provider integrations are adapters behind `runner.Adapter`. The catalog,
picker, workflow state machine, session persistence, and command handlers use
that interface and contain no provider-specific invocation logic. A new CLI
adds its adapter plus one factory registration in the runner registry; its
validation and capability probe stay beside that boundary. Registry tests use
a fake future provider to keep this extension point executable rather than
documentation-only.
