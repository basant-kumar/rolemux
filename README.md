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
go install github.com/basant-kumar/rolemux/cmd/rolemux@latest
```

## Set up

Install the RoleMux skill into supported host agents, check provider logins, and
choose models:

```bash
rolemux install --global --hosts all
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
may span multiple host invocations.

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
   dependency graph; trivial work is one implementation-and-test unit.
2. Each review command makes one reviewer turn and at most one requested
   revision or fix, then returns control for an explicit next review.
3. Independent work units may run concurrently in the shared checkout.
4. Each implementer and reviewer resumes its own provider session.
5. One logical deep integration review runs after all work units pass; its
   durable reviewer/fixer sessions can continue across host invocations.

For an obvious local fix, skip the planner and plan-reviewer calls:

```bash
rolemux quick start --task "Keep the active role visible in configure" \
  --scope 'internal/cli/configure_view.go,internal/cli/configure_view_test.go' --json
rolemux implement <task-id> --json
rolemux code review <task-id> --json
```

`quick start` performs no provider/model discovery and requires an explicit
narrow scope. Full plans expose `complexity` as `trivial`, `small`, `medium`,
`large`, or `system`; RoleMux rejects over-decomposed graphs and prose-like
scope entries. A one-unit trivial/small plan does not repeat its focused code
review as a broad integration-model call.

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

RoleMux keeps context useful and small: the planner sends self-contained work
packets, resumed turns send only new answers or diffs, task reviews inspect the
change and its blast radius, and one logical full integration review gate runs
across the approved work units. Review results expose compact status,
review-kind, round, limit, reviewability, and next-action fields; exit 0 means
the operation completed, not that the review approved.
`rolemux usage <task-id> --json` reports measured usage when providers expose
it. If [pxpipe](https://github.com/teamchong/pxpipe) is installed and eligible,
RoleMux can use it automatically to try to save more tokens.

## Useful commands

| Command | Purpose |
|---|---|
| `rolemux help` | Show all commands |
| `rolemux configure --global` | Configure role profiles |
| `rolemux models --json` | List discovered models for agents |
| `rolemux doctor --json` | Check CLIs, logins, models, and installation |
| `rolemux list --json` | List tasks |
| `rolemux status <task-id> --json` | Inspect workflow state |
| `rolemux usage <task-id> --json` | Compare measured token usage |
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
