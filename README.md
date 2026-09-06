# RoleMux

[![Release](https://img.shields.io/github/v/release/basant-kumar/rolemux?sort=semver)](https://github.com/basant-kumar/rolemux/releases/latest)
[![CI](https://github.com/basant-kumar/rolemux/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/basant-kumar/rolemux/actions/workflows/ci.yml)
[![Downloads](https://img.shields.io/github/downloads/basant-kumar/rolemux/total)](https://github.com/basant-kumar/rolemux/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/basant-kumar/rolemux)](https://go.dev/)
[![License](https://img.shields.io/github/license/basant-kumar/rolemux)](LICENSE)
[![macOS](https://img.shields.io/badge/macOS-supported-007AFF?logo=apple&logoColor=white)](#install)

**The right mind for every role.**

RoleMux lets your current CLI agent use different models for planning,
implementation, and review. Each model keeps its provider session, so questions
and review fixes resume with context instead of starting over.

Your Claude Code, Codex, Copilot, or Antigravity session remains the
orchestrator. RoleMux supplies the configured specialists.

## Install

Tap the repository once, then use the short package name:

```bash
brew tap basant-kumar/tap
brew install --cask rolemux
```

Future operations stay short:

```bash
brew upgrade --cask rolemux
```

The distributed binaries are Developer ID signed and Apple-notarized. To build
from source instead, use Go 1.24 or newer:

```bash
mkdir -p $HOME/.local/bin
go env -w GOBIN=$HOME/.local/bin
export PATH="$HOME/.local/bin:$PATH"
go install github.com/basant-kumar/rolemux/cmd/rolemux@latest
```

## Set up

The host-agent installation step is mandatory; without it, host agents may not
discover or correctly orchestrate RoleMux:

```bash
rolemux install --global --hosts all
```

Then check provider logins and choose models:

```bash
rolemux doctor --json
rolemux configure --global
```

The picker is searchable and keyboard-driven. It discovers models from your
installed provider CLIs, uses the cache immediately, and refreshes it in the
background.

To change only one role later:

```bash
rolemux configure --global --role planner
rolemux configure --global --role implementer
rolemux configure --global --role reviewer
```

Plan and code reviewers share the reviewer profile unless you configure
`plan-reviewer` or `code-reviewer` separately. Global configuration lives at
`~/.rolemux/config.toml`; a repository can override it with `.rolemux.toml`.
Every provider, model, effort, speed, verification, login, and discovery screen
keeps a highlighted `Role: …` badge visible, plus the current step and selected
upstream settings.

Review continuation is host-controlled. Each `plan review`, `code review`, or
integration review command performs one reviewer turn and, when changes are
requested, at most one revision or fix before returning control. Run another
explicit review command to review that revised or fixed result. `plan answer`,
`implement answer`, and `retry` also return after their completed operation;
they do not start another review automatically. Integration is one logical,
broad review gate with dedicated durable reviewer and fixer sessions, so it
may span multiple host invocations. Reviewer approval of the plan and final
integrated code creates a hard human gate with Approve, Request changes, and
Discuss choices.

At the final code gate, `approval show` prints narrow local Git review commands.
For a GitHub repository, you can explicitly publish a temporary draft PR and
send its comments back through the same implementation session:

```bash
rolemux approval publish <task-id>
# Comment on the draft PR.
rolemux approval sync <task-id>
```

Publishing is opt-in, uses the existing `gh` login, never touches the current
branch/index/worktree, and reuses one draft PR across fix/re-review rounds. The
PR is a review surface only—do not merge it. If GitHub or `gh` is unavailable,
review locally and use `approval respond` for feedback or approval.

The review safety limit is a top-level setting. The default is five accepted
reviewer verdicts per review task:

```toml
review_max_rounds = 5
```

Set it from the CLI for future tasks with
`rolemux configure --global --review-max-rounds N` (or `--project`); any
nonnegative integer is accepted:

```bash
rolemux configure --global --review-max-rounds 0   # unlimited ceiling
rolemux configure --project --review-max-rounds 12 # custom positive limit
```

Zero removes the ceiling but not the one-reviewer-turn/one-fix boundary of a
single invocation. Interactive `rolemux configure` includes a `Review safety
limit` entry with current, default (5), 10, and unlimited choices. When
`ROLEMUX_CONFIG` is set, RoleMux uses that file alone and skips normal
project/global discovery. When it is unset, project `.rolemux.toml` overlays
global `~/.rolemux/config.toml`. Its effective value is snapshotted into each
new task; work units and the derived integration task inherit that snapshot,
and later configuration edits do not change existing tasks. Historical tasks
keep a positive saved `max_rounds`; older tasks without one default to five.

## Use it

Tell the current agent to use RoleMux in your task prompt:

```text
Use RoleMux to implement resumable uploads.
```

The orchestrator then drives the workflow:

1. For a local change with a known scope, use the planner-free fast path below.
   Otherwise, the planner classifies complexity and produces the smallest valid
   dependency graph and shortest safe critical path; trivial work is one
   implementation-and-test unit.
2. After the plan reviewer approves, you approve or steer the reviewed plan.
3. Independent work units may run concurrently in the shared checkout; each
   context lane resumes its implementer session, while disjoint lanes may run
   concurrently.
4. Unit reviews advance automatically; one integration review follows all
   units and then stops for your final code approval.

For an obvious local fix, skip the planner and plan-reviewer calls:

```bash
rolemux quick start --task "Keep the active role visible in configure" \
  --scope 'internal/cli/configure_view.go,internal/cli/configure_view_test.go' --json
rolemux implement <task-id> --json
rolemux code review <task-id> --json
rolemux approval show <task-id>
```

`quick start` performs no provider/model discovery and requires an explicit
narrow scope. Full plans expose `complexity` as `trivial`, `small`, `medium`,
`large`, or `system`; RoleMux rejects over-decomposed graphs and prose-like
scope entries. Planner packets include authoritative files, symbols, estimates,
dependencies, and narrow validation so implementers do not rediscover the
repository. A one-unit trivial/small plan does not repeat its focused code
review as a broad integration-model call, but it still requires final human
approval.

RoleMux never silently changes the selected provider, model, effort, or speed.
If a provider is unavailable or rate-limited, it returns control to the
orchestrator.

## Supported providers

| Provider | CLI | Notes |
|---|---|---|
| Claude Code | `claude` | Planning, implementation, and review |
| Codex | `codex` | Planning, implementation, review, and optional pxpipe transport |
| Antigravity | `agy` | Native model variants, effort, sandboxing, and session resume |
| GitHub Copilot | `copilot` | SDK-backed roles; implementation gets repository-scoped edits |

RoleMux uses existing CLI logins. If a selected provider is signed out, it
starts that provider's login flow and discovers models after login completes.

## Token use

RoleMux sends self-contained work packets, resumes provider sessions, reviews
only task deltas and their direct blast radius, then runs one integration review
per plan. It streams compact progress to stderr and persists model turns, tool
calls, provider invocations, and reported tokens. Per-role budgets stop runaway
turns, tools, time, and output; a human can inspect and explicitly extend them.

`plan graph --json` is compact; add `--full` only when packet details are needed.
If [pxpipe](https://github.com/teamchong/pxpipe) is installed, RoleMux reports
whether each eligible turn actually used image compression or passed through as
text. The installed pxpipe configuration—not RoleMux—decides model eligibility.

## Useful commands

| Command | Purpose |
|---|---|
| `rolemux help` | Show all commands |
| `rolemux configure --global` | Configure role profiles |
| `rolemux models --json` | List discovered models for agents |
| `rolemux doctor --json` | Check CLIs, logins, models, and installation |
| `rolemux list --json` | List tasks |
| `rolemux status <task-id> --json` | Inspect workflow state |
| `rolemux plan graph <task-id> --json` | Inspect compact scheduling state |
| `rolemux approval show <task-id>` | Inspect a pending human gate |
| `rolemux approval publish <task-id>` | Publish or update an optional GitHub draft review |
| `rolemux approval sync <task-id>` | Import new PR comments as requested changes |
| `rolemux approval respond …` | Approve, request changes, or discuss |
| `rolemux usage <task-id> --json` | Compare measured token usage |
| `rolemux budget show <task-id>` | Inspect snapshotted role budgets |
| `rolemux budget extend …` | Explicitly extend an exhausted budget |
| `rolemux work adopt …` | Adopt scoped host fallback changes for review |
| `rolemux retry <task-id> --json` | Resume a recoverable provider turn |

## Documentation

- [CLI and workflow reference](docs/reference.md)
- [Release and signing guide](docs/releasing.md)

## Development

```bash
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
```

Provider integrations are modular adapters behind `runner.Adapter`; adding a
CLI does not require changes to the workflow state machine.

## License

RoleMux is licensed under the [Apache License 2.0](LICENSE). See
[NOTICE](NOTICE) for attribution.
