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
5. Run `rolemux implement <task-id> [--scope <paths-or-globs>] --json`, then
   `rolemux code review <task-id> --json`. Code-review findings return to the
   same implementer session before re-review.
6. Inspect `rolemux status <task-id> --json` whenever recovery is needed. Use
   `rolemux retry <task-id> --json` only when status reports a retryable saved
   operation.

The orchestrator may run independent task IDs concurrently in the same checkout.
Choose scopes that describe each task's intended files. Scope overlap is a
warning for the orchestrator, not a RoleMux scheduling decision.

Keep orchestration token-conscious: pass task IDs instead of replaying earlier
provider output, rely on RoleMux's resumed sessions, and avoid calling `status`
or refreshing `models` unless the workflow or recovery actually requires it.
Never omit new user constraints or evidence needed for correctness.

Use `rolemux usage <task-id> --json` for a compact per-role token comparison;
do not load full task state merely to inspect consumption.

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
in a terminal for the searchable model picker.
