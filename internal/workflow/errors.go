package workflow

import (
	"errors"
	"fmt"

	"github.com/basant-kumar/rolemux/internal/task"
)

const (
	ExitOK           = 0
	ExitUsage        = 2
	ExitNeedsInput   = 3
	ExitReviewNeeded = 4
	ExitAction       = 5
	ExitInFlight     = 6
	ExitExhausted    = 7

	ApprovalRequiredCode = "APPROVAL_REQUIRED"
	ApprovalConflictCode = "APPROVAL_CONFLICT"
	ApprovalStaleCode    = "APPROVAL_STALE"
	ApprovalArtifactCode = "APPROVAL_ARTIFACT"
	ApprovalRecoveryCode = "APPROVAL_RECOVERY_REQUIRED"
)

type Error struct {
	Code      string
	Message   string
	Retryable bool
	TaskID    string
	ExitCode  int
	Cause     error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Code
}

func (e *Error) Unwrap() error { return e.Cause }

func problem(code, message, id string, exit int, retryable bool, cause error) error {
	return &Error{Code: code, Message: message, TaskID: id, ExitCode: exit, Retryable: retryable, Cause: cause}
}

func classify(id string, err error) error {
	if err == nil {
		return nil
	}
	var own *Error
	if errors.As(err, &own) {
		return err
	}
	switch {
	case errors.Is(err, task.ErrOperationInFlight):
		return problem("OPERATION_IN_FLIGHT", "a task operation is already in flight", id, ExitInFlight, true, err)
	case errors.Is(err, task.ErrStaleOperation):
		return problem("STALE_OPERATION", "a stale provider completion was discarded", id, ExitAction, true, err)
	case errors.Is(err, task.ErrScopeChanged):
		return problem("REVIEW_NEEDED", "scoped files changed during code review", id, ExitReviewNeeded, true, err)
	case errors.Is(err, task.ErrNotFound):
		return problem("TASK_NOT_FOUND", fmt.Sprintf("task %q was not found", id), id, ExitUsage, false, err)
	case errors.Is(err, task.ErrTaskExists):
		return problem("TASK_EXISTS", fmt.Sprintf("task %q already exists", id), id, ExitUsage, false, err)
	case errors.Is(err, task.ErrInvalidTaskID):
		return problem("INVALID_TASK_ID", "task ID must be a path-safe slug", id, ExitUsage, false, err)
	case errors.Is(err, task.ErrInvalidPhase):
		return problem("INVALID_TASK_STATE", "command is not valid in the task's current phase", id, ExitUsage, false, err)
	default:
		return problem("INTERNAL", err.Error(), id, ExitAction, false, err)
	}
}

func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var e *Error
	if errors.As(err, &e) && e.ExitCode != 0 {
		return e.ExitCode
	}
	return ExitAction
}
