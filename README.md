# RoleMux

**The right mind for every role.**

Plan with one model. Build with another. Review with a third—without making any
of them start from zero.

RoleMux is a thin, host-friendly CLI that lets Claude Code, Codex, GitHub
Copilot CLI, Antigravity, or another orchestrator assign different provider
models to planning, implementation, and review. Each role keeps its exact
provider session across questions and review fixes, so context is resumed
instead of reconstructed.

RoleMux does not replace the host agent. The host remains the orchestrator and
decides when to plan, implement, review, retry, or run independent tasks in
parallel.

## Why RoleMux

- Use the strongest reasoning model for architecture without paying its cost
  for every implementation turn.
- Give implementers self-contained execution packets and declared write scopes.
- Resume the same planner, implementer, and reviewer sessions through questions
  and fix loops.
- Run independent planner-produced work units concurrently in one shared
  checkout, then perform one deep integration review.
- Discover models from installed CLI accounts through a fast cached picker,
  while refreshing provider catalogs in the background.
- Keep JSON stdout stable for agents and human diagnostics on stderr.

## Quick start

```bash
ROLEMUX_MODULE="github.com/basant-kumar/rolemux"
go install "${ROLEMUX_MODULE}/cmd/rolemux@latest"

rolemux install --global --hosts all
rolemux doctor --json
rolemux configure --global
```

Then ask the current Claude Code, Codex, Copilot, or Antigravity session to use
RoleMux. The bundled host skill teaches it how to drive planning, parallel work
units, bounded review loops, questions, retries, and integration review.

## Token discipline

RoleMux treats tokens as a resource, without silently dropping context needed
for correctness. A role receives the complete task context on its first turn;
later turns resume that exact provider session and send only new answers,
review findings, revised artifacts, or scoped deltas. Prompts tell workers to
inspect only relevant files and to avoid restating inputs. Strict small JSON
envelopes, task scopes, bounded process output, and the five-round review cap
prevent accidental context growth.

When `pxpipe` is executable (or `PXPIPE_CLI_PATH` names it), RoleMux starts a
private foreground pxpipe server for each eligible Claude or Codex task turn;
no separately managed daemon is required. Authentication, version checks,
probes, and model discovery still call each provider directly. If pxpipe is
missing, RoleMux suggests `npm install --global pxpipe-proxy` once on the first
turn of each new eligible provider session, then runs the provider directly and
stays quiet on that session's resumes. If pxpipe cannot start before the
provider accepts the task, RoleMux reports the reason and runs the original
provider command directly. Codex task turns
first run the selected Codex executable's direct
`login status` check on every fresh or resumed turn. Only positively verified
ChatGPT authentication with the default or explicitly equivalent
`https://chatgpt.com/backend-api/codex` route gets an ephemeral Responses
transport overlay and a private foreground pxpipe server. API-key,
unauthenticated, unknown, conflicting, custom-route, missing-helper, or
startup-failed cases run the original Codex command directly. Direct fallback
never replays a task after Codex has emitted its durable `thread.started`
event.

RoleMux does not maintain a pxpipe model list or claim compression. pxpipe
retains its own exact model allowlist and decides whether the selected model is
image-eligible or passes through as text. RoleMux always preserves the selected
model name. In installed pxpipe 0.13.2, an explicitly allowlisted Luna uses the
generic GPT-5.x profile rather than Sol's tuned profile; Astra has no declared
family profile and would fall into an unverified default. RoleMux therefore
does not auto-enable either. Add an unlisted model only after a controlled
quality/token comparison or an upstream model profile. `pxpipe stats --file <events-file>` is the authority on
measured savings. While a turn is running, RoleMux prints its temporary
dashboard URL and event-log path as a launch diagnostic.

RoleMux does not guess at token counts, truncate plans, or replace required
evidence with a summary. Provider/model token limits configured for gateways
are honored by their adapters. `rolemux usage <task-id> --json` reports
per-role provider requests, prompt bytes, and input/cache/output/reasoning/total
tokens whenever the selected provider exposes usage data.

## Status

RoleMux v1 is macOS-first and built as a single Go binary. It supports:

- separate planner, implementer, shared reviewer, plan-reviewer override, and
  code-reviewer override profiles;
- account-aware model discovery with a cached-first interactive picker,
  background refresh, explicit live refresh, and machine-readable JSON;
- a searchable, full-screen model, effort, and speed picker with live model
  descriptions and context limits when the provider exposes them;
- durable provider sessions and restart-safe questions/retries;
- automatic plan-review and code-review fix loops, capped at five rounds;
- planner-produced, validated dependency graphs with safe parallel waves,
  independently resumable work-unit tasks, and a one-time deep integration
  review/fix loop;
- multiple task IDs running concurrently in one shared checkout;
- global defaults with project-local TOML overrides;
- existing CLI authentication and explicitly configured gateways/BYOK;
- installable host skills for Claude Code, Codex, GitHub Copilot CLI, and
  Google Antigravity CLI;
- bounded provider-aware skill/tool inventories and host-mediated capability
  handoffs on the first turn of every delegated role;
- graceful interrupt recovery and a configurable provider-turn timeout.

Google Antigravity is a supported provider through its installed `agy` CLI,
including live model discovery, model/effort pinning, sandboxed plan/review and
implementation modes, durable conversation resume, and provider-reported token
usage. Antigravity model variants that encode an effort expose only that effort;
models that reject `--effort` do not show an effort screen.

Copilot models can plan, implement, and review through the pinned Copilot SDK.
Only the implementation role receives Copilot's `edit` tool, and each write is
checked against the repository root, symlink boundaries, and the task's declared
scope. Shell access remains disabled. RoleMux never silently switches to another
model or provider.

## Install on macOS

Homebrew is the recommended installation path and does not require a local Go
toolchain:

```bash
brew install --cask basant-kumar/tap/rolemux
```

Starting with v0.1.1, the release pipeline refuses to publish unless binaries
are signed with an Apple Developer ID and accepted by Apple's notarization
service. Users installing archives manually should also verify their GitHub
attestation as documented below.

Then install the host-agent skill and verify provider readiness:

```bash
rolemux help
rolemux install --global --hosts all
rolemux doctor --json
rolemux configure --global
```

Developers with Go 1.24 or newer can instead install the latest tagged source
release directly:

```bash
ROLEMUX_MODULE="github.com/basant-kumar/rolemux"
go install "${ROLEMUX_MODULE}/cmd/rolemux@latest"
```

Go installs the binary in `GOBIN`, or in `GOPATH/bin` when `GOBIN` is unset.
Resolve that directory and add it to the current shell path:

```bash
ROLEMUX_BIN_DIR="$(go env GOBIN)"
[ -n "$ROLEMUX_BIN_DIR" ] || ROLEMUX_BIN_DIR="$(go env GOPATH)/bin"
export PATH="$ROLEMUX_BIN_DIR:$PATH"
```

After installation, run `rolemux help` to discover supported commands.

Add the same `export PATH=...` setting to `~/.zshrc` to make it persistent in
new terminals.

From an existing source checkout, install the current revision with:

```bash
go install ./cmd/rolemux
```

`go.mod` is the canonical public module identity. Go import declarations must
use that literal module path at compile time; runtime/documentation values that
can be centralized are kept in shared constants or variables.

## Maintainer releases

The canonical repository is
[`github.com/basant-kumar/rolemux`](https://github.com/basant-kumar/rolemux).
It is public and licensed under Apache-2.0. The release workflow uses
GoReleaser 2.18.0 to build macOS ARM64 and Intel archives, generate SHA-256
checksums and per-archive SBOMs, create GitHub-native release notes, update the
public `basant-kumar/homebrew-tap` Cask, and submit GitHub artifact attestations.
The npm registry is intentionally not a v0.1 distribution channel.

The repository secret `HOMEBREW_TAP_GITHUB_TOKEN` must contain a token with
content-write access to the tap repository. macOS release signing additionally
requires these GitHub Actions secrets:

- `MACOS_SIGN_P12`: base64-encoded Developer ID Application certificate and
  private key exported as a password-protected `.p12` file;
- `MACOS_SIGN_PASSWORD`: the `.p12` export password;
- `MACOS_NOTARY_KEY`: base64-encoded App Store Connect API `.p8` key;
- `MACOS_NOTARY_KEY_ID`: the API key ID;
- `MACOS_NOTARY_ISSUER_ID`: the API issuer UUID.

The release workflow fails before GoReleaser if any signing secret is absent,
so an unsigned Cask cannot be published accidentally. Review and publish a
release with:

```bash
ROLEMUX_REPOSITORY="basant-kumar/rolemux"
ROLEMUX_MODULE="github.com/${ROLEMUX_REPOSITORY}"
ROLEMUX_VERSION="v0.1.1"

go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
HOMEBREW_TAP_OWNER="${ROLEMUX_REPOSITORY%%/*}" \
  HOMEBREW_TAP_REPOSITORY="homebrew-tap" \
  ROLEMUX_REPOSITORY_NAME="${ROLEMUX_REPOSITORY##*/}" \
  goreleaser release --snapshot --clean --skip=publish
git status --short
# Review the listed files, then stage only the intended release contents.
git add <release-files>
git commit -m "release: ${ROLEMUX_VERSION}"
git push origin main

git tag -a "${ROLEMUX_VERSION}" -m "RoleMux ${ROLEMUX_VERSION}"
git push origin "${ROLEMUX_VERSION}"
```

Pushing the tag is the only release trigger; do not also create the GitHub
release manually. Verify the completed workflow and its downloadable artifacts
before announcing the version:

```bash
gh run list --repo "${ROLEMUX_REPOSITORY}" --workflow Release --limit 1
gh release view "${ROLEMUX_VERSION}" --repo "${ROLEMUX_REPOSITORY}"
```

Verify the public module without replacing the active installation:

```bash
ROLEMUX_VERIFY_BIN="$(mktemp -d)"
GOBIN="${ROLEMUX_VERIFY_BIN}" \
  go install "${ROLEMUX_MODULE}/cmd/rolemux@${ROLEMUX_VERSION}"
"${ROLEMUX_VERIFY_BIN}/rolemux" version
```

For a downloaded archive, verify its GitHub provenance before running it:

```bash
gh attestation verify <rolemux-archive.tar.gz> --repo "${ROLEMUX_REPOSITORY}"
```

RoleMux uses installed provider CLIs and their existing logins. The configure
picker renders installed providers immediately in the order Claude, Codex,
Antigravity, and Copilot; it does not run a blocking authentication sweep.
Selecting a provider verifies only that provider. If authentication is needed,
RoleMux temporarily leaves the picker and runs its official login (`claude auth
login`, `codex login`, `copilot login`, or interactive `agy`). On later visits
the model screen opens from the account-scoped cache immediately and refreshes
that provider in the background for the next visit. A missing CLI stays in the
provider screen with installation guidance; on macOS, install GitHub Copilot
CLI with `brew install copilot-cli` and select it again.

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

The first screen selects `All roles`, `Planner`, `Implementer`, `Shared
reviewer`, `Plan reviewer`, or `Code reviewer`. Choosing one role updates only
that profile and preserves every other global or project setting.

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

- global: `$HOME/.rolemux/config.toml`
- project: `<repository>/.rolemux.toml`
- explicit replacement: `ROLEMUX_CONFIG=/absolute/path/to/config.toml`

Example TOML:

```toml
provider_turn_timeout_seconds = 900

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
`provider_turn_timeout_seconds` accepts 30–7200 seconds. A timed-out turn with
a known session becomes a retryable operation that resumes that exact session.

## Discover models

Inspect the human-readable catalog:

```bash
rolemux models
rolemux models --runner claude
rolemux models --runner copilot --refresh
rolemux models --runner antigravity --refresh
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
installed CLI's SDK model list. Antigravity comes from `agy models`, with
provider-native model slugs and effort constraints inferred from its explicit
model variants. Cached results include their age. The interactive picker
preserves the snapshot's last verified availability while clearly labeling its
cached origin; an explicit failed live refresh degrades fallback availability
to `unknown`. `--json` includes
structured effort and speed options plus context limits when advertised.

## Skills, tools, and host handoffs

Before the first turn of each planner, implementer, plan reviewer, and code
reviewer session, RoleMux supplies a compact capability inventory. It contains
only bounded `name`/`description` skill metadata, provider/scope attribution,
the selected harness's available tool names, and installed-helper status. It
never copies skill bodies, arbitrary frontmatter, environment values, or
credentials. Resumed turns do not repeat the inventory.

Discovery follows each provider's precedence instead of applying one global
rule. It covers provider-native project/personal roots, shared `.agents/skills`
roots where supported, and bounded installed-plugin skill roots. Codex scans
the repository root and user `.agents/skills` plus `/etc/codex/skills`; it may
surface same-named skills from multiple scopes. Legacy `.codex/skills` entries
are metadata-only and are not labeled native. Claude runs
with its `Skill` tool while retaining restricted file/tool permissions and an
empty MCP allowlist. Copilot SDK sessions load only RoleMux's explicit skill
directories; ambient config, hooks, MCP servers, and shell remain off. Its
implementer alone receives scope-confined workspace edits.
Codex and Antigravity retain their native skill discovery and sandboxes.

Inventory metadata does not grant a tool permission. If a worker needs a UI,
connector, or external-data capability available only in the host
orchestrator, the planner/implementer returns a precise `needs_input` question.
The host uses its applicable skill or read-only tool and returns concise
evidence through `plan answer` or `implement answer`, preserving the worker's
session. A reviewer requests the same evidence as a finding, which reaches the
planner or implementer through the existing review loop.

The upstream [pxpipe README](https://github.com/teamchong/pxpipe#pxpipe-warp)
lists `claude`, `cursor-agent`, `codex`, and shell aliases as `warp` launch
examples. Its default route covers Anthropic Messages; Codex's Responses path
needs RoleMux's explicit
`chatgpt.com/backend-api/codex/responses*=http://127.0.0.1:<port>` route and a
foreground server whose OpenAI upstream is `https://chatgpt.com`. RoleMux
does not infer eligibility from executable discovery or protocol support. The
private lifecycle chooses bounded private candidate ports, requires pxpipe's own
listening announcement plus an HTTP readiness check, and stops only the private
process tree it started. It never reuses or reconfigures a user daemon. The
tested versions are
pxpipe 0.13.2 and Codex 0.153.3; an incompatible or unavailable helper is
optional and leaves the direct provider turn functional. The installed pxpipe
configuration remains the runtime authority for model eligibility and text
pass-through. Each reported dashboard URL exists only for that provider turn;
the event file remains available afterward.

### Optional pxpipe smoke check

This is an opt-in measurement procedure. It does not log in, change an
existing Codex credential, or make fake tests into compression evidence.

1. Confirm `pxpipe --version` is `0.13.2` and `codex --version` is
   `0.153.3`, and use existing ChatGPT credentials.
2. Run one normal RoleMux task turn with `PXPIPE_LOG` set to a disposable
   event-log path. Complete a fresh turn and then a question/retry or review
   resume in the same task; verify the same Codex session continues.
3. Inspect the diagnostic event-log path with
   `pxpipe stats --file <events-file>`. A successful real compression run
   should show a Responses request with status 200, `compressed: true`, and
   PNGs. A missing event means the request was unwrapped or did not match the
   route and must be diagnosed; it does not verify pass-through.
4. Repeat with a disposable `PXPIPE_CONFIG` containing `{"models":"off"}`.
   The exact same model and session should complete, and `pxpipe stats
   --file <events-file>` must show the Responses request with status 200 and
   uncompressed/text pass-through (not a missing event). This verifies that
   pxpipe handled the request while its image arm was disabled.
5. Separately use a stored Codex API-key login without an API-key environment
   variable. Verify the task completes through the direct Codex path and no
   pxpipe event is expected. Restore the original environment/configuration
   after the check.

## Run a workflow

```bash
rolemux plan start --task "Add resumable uploads" --json
rolemux plan review <task-id> --json
rolemux plan graph <task-id> --json
```

The graph response contains deterministic topological `waves`, live `ready`
unit IDs, exact write scopes, dependencies, and self-contained execution
packets. For every currently ready unit, the orchestrator may concurrently run:

```bash
rolemux work start <task-id> <unit-id> --json
rolemux implement <returned-work-task-id> --json
rolemux code review <returned-work-task-id> --json
```

Each work unit has independent RoleMux state, provider sessions, and a short
state lock, so same-wave units can run in parallel while seeing one another's
disjoint changes in the shared checkout. Re-run `plan graph` after approvals to
discover newly ready nodes. After all nodes are approved, run the one-time deep
integration gate:

```bash
rolemux work integrate <task-id> --json
```

The integration reviewer starts fresh with the aggregate plan and delta. If it
requests changes, RoleMux starts one fresh integration implementer, sends all
cross-unit findings there, and resumes that same fixer plus the same reviewer
until approval or the five-round cap. The host orchestrator schedules and
answers questions but does not bypass the configured implementation model by
editing integration fixes itself.

The generated plan is written locally to `.rolemux/plans/<task-id>.md`. The
entire `.rolemux/` directory is ignored and must not be committed. Private
task/session state lives under `git rev-parse --git-path rolemux`, normally
`.git/rolemux`, so it also cannot become a tracked project artifact.

When a planner or implementer asks a question, RoleMux returns it to the host:

```bash
rolemux plan answer <task-id> --answer "Use multipart uploads" --json
rolemux implement answer <task-id> --answer "Keep the existing API" --json
```

The answer resumes the exact same provider session. Review findings likewise go
back to the same planner or implementer session, then return to the same reviewer
session until approved or the five-round limit is reached.
Ctrl+C/SIGTERM and the provider-turn timeout cancel the child through RoleMux's
context, save a retry when a durable session is known, and avoid leaving a new
stale in-flight operation. If RoleMux itself is killed before it can save that
retry, the in-flight record includes its owner process. A later `rolemux retry`
atomically recovers the abandoned turn and resumes its exact durable provider
session. Legacy ownerless records become recoverable after the configured turn
timeout plus a short grace period. RoleMux refuses recovery when no durable
session exists rather than risking duplicate work in a fresh conversation.

Recovery commands:

```bash
rolemux status <task-id> --json
rolemux status <task-id> --full --json
rolemux usage <task-id> --json
rolemux retry <task-id> --json
rolemux list --json
```

`status --json` is compact by default: it includes phase, rounds, selected
profiles, usage, findings, questions, and recovery/session metadata, but omits
large prompts, plans, and manifests. Use `--full` only for deep diagnostics.
`rolemux usage` is the compact token comparison view. It reports measured
request counts and prompt bytes plus provider-reported input, cache, output,
reasoning, and total tokens for each delegated role. Host-orchestrator usage is
not available through provider CLIs and is therefore not estimated.

## Shared-checkout concurrency

RoleMux does not create worktrees or impose a repository-wide lock. The
orchestrator may run independent graph nodes or independent task IDs
concurrently in the same checkout so agents see one another's changes. Only
short task-state updates are locked. RoleMux rejects a planner graph that puts
overlapping write scopes in parallel; overlap is allowed only when a dependency
orders the nodes.

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
- `~/.agents/skills/rolemux/SKILL.md` (Codex and other compatible agents)
- `~/.copilot/skills/rolemux/SKILL.md`
- `~/.gemini/antigravity-cli/skills/rolemux/SKILL.md`

An identical existing file is an idempotent success. A different file is never
overwritten unless `--force` is explicitly supplied.

## Security model

- Provider model/effort/speed/routing choices are snapshotted when a task is created.
- Secrets are never written to task state, catalog caches, argv, or diagnostics.
- Codex and Claude runs use explicit non-interactive sandbox/tool restrictions;
  enabling native skills does not broaden their write or MCP permissions.
- Copilot uses SDK empty mode with repository-confined `view`/`grep` reads,
  `web_fetch`, and explicit skill directories. Only its implementer receives
  `edit`, with each write confined to the declared task scope; shell, MCP,
  hooks, memory, extensions, and subagents remain denied.
- Provider/version capability mismatches fail closed with an actionable error.
- Interactive and direct configuration validate model, effort, speed, and role
  support against the live provider catalog. Task creation revalidates the
  snapshotted tuple, and adapters reject a reported model/effort mismatch.

Run `rolemux doctor --json` to inspect CLI versions, authentication, selected
model/effort/speed compatibility, paths, permissions, skill integrity, and
writable runtime directories without exposing credential values.

## Adding another provider CLI

Provider integrations are adapters behind `runner.Adapter`. The catalog,
picker, workflow state machine, session persistence, and command handlers use
that interface and contain no provider-specific invocation logic. A new CLI
adds its adapter plus one factory registration in the runner registry; its
validation and capability probe stay beside that boundary. Registry tests use
a fake future provider to keep this extension point executable rather than
documentation-only.

Task transport helpers use a separate provider-scoped lifecycle interface.
This keeps helpers such as pxpipe out of authentication and discovery calls and
allows a future provider to add validated endpoint routes without changing
workflow state or the provider adapter contract.

## License

RoleMux is licensed under the [Apache License 2.0](LICENSE). See
[NOTICE](NOTICE) for attribution.
