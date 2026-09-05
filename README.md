# RoleMux

[![Release](https://img.shields.io/github/v/release/basant-kumar/rolemux?sort=semver)](https://github.com/basant-kumar/rolemux/releases/latest)
[![CI](https://github.com/basant-kumar/rolemux/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/basant-kumar/rolemux/actions/workflows/ci.yml)
[![Downloads](https://img.shields.io/github/downloads/basant-kumar/rolemux/total)](https://github.com/basant-kumar/rolemux/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/basant-kumar/rolemux)](https://go.dev/)
[![License](https://img.shields.io/github/license/basant-kumar/rolemux)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-macOS-000000?logo=apple&logoColor=white)](#install)

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

## Use it

Tell the current agent to use RoleMux in your task prompt:

```text
Use RoleMux to implement resumable uploads.
```

The orchestrator then drives the workflow:

1. The planner produces an implementation plan and dependency graph.
2. Plan review loops until approval or the five-round limit.
3. Independent work units may run concurrently in the shared checkout.
4. Each implementer and reviewer resumes its own provider session.
5. One deep integration review runs after all work units pass.

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
change and its blast radius, and the full integration review runs once.
`rolemux usage <task-id> --json` reports measured usage when providers expose
it. If pxpipe is installed and eligible, RoleMux can use it automatically and
prints the temporary dashboard URL.

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
