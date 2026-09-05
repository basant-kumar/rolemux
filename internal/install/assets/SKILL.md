---
name: rolemux
description: Orchestrate planning, implementation, and review through RoleMux when a task should use configured models while preserving provider sessions and bounded review loops.
---

# RoleMux

Use the `rolemux` binary as the workflow driver while remaining the user-facing
orchestrator. RoleMux selects the configured provider/model for each role and
persists the exact provider session used by that role.

## Run a task

1. Run `rolemux doctor --json` when provider readiness is unknown. If it reports
   a missing CLI or login, return that action to the user; do not silently choose
   another model.
2. Start planning with `rolemux plan start --task "<task>" --json`. Retain the
   returned task ID for every later command.
3. When a command exits `3`, present its exact question to the user. Resume with
   `rolemux plan answer <task-id> --answer "<answer>" --json` or
   `rolemux implement answer <task-id> --answer "<answer>" --json`, according
   to `pending_question_source`.
4. Run `rolemux plan review <task-id> --json`. RoleMux sends requested changes
   back to the same planner session and reuses the same reviewer session, up to
   its configured five-round limit.
5. Read `rolemux plan graph <task-id> --json`. For every ID in `ready`, run
   `rolemux work start <task-id> <unit-id> --json`, then use the returned work
   task ID with `rolemux implement <work-task-id> --json` and `rolemux code
   review <work-task-id> --json`. Same-wave units may run concurrently; do not
   start blocked units. Re-read the compact graph after approvals.
6. After every graph node is approved, run `rolemux work integrate <task-id>
   --json`. This starts one fresh deep reviewer and, only if needed, one fresh
   integration fixer. RoleMux resumes those two sessions through the bounded
   fix/review loop.
7. Inspect `rolemux status <task-id> --json` whenever recovery is needed. Use
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

## Interpret results

- Treat JSON stdout as the sole machine-readable result; diagnostics are on
  stderr.
- Exit `0` means the command completed. Exit `3` requires a user answer. Exit
  `4` means scoped files changed during review and the saved review should be
  retried. Exit `5` requires orchestrator action, such as login or an available
  configured provider. Exit `6` means the same task already has an operation in
  flight. Exit `7` means the five-round plan or code-review limit was exhausted.
- Never infer approval from prose or continue past a failed review gate.
- RoleMux workers do not commit, push, stash, reset, rebase, or merge. Perform
  repository and build commands separately, within the user's authorization.

Use `rolemux models --json` for agent-friendly discovery or `rolemux configure`
in a terminal for the searchable model picker. The picker reads its
account-scoped cache immediately and refreshes the selected provider in the
background; request `models --refresh` only when a blocking live result is
specifically required.
