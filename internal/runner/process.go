package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultMaxOutputBytes     int64 = 16 << 20
	processGracefulStopWindow       = 500 * time.Millisecond
)

type ProcessSpec struct {
	Path           string
	Args           []string
	Dir            string
	Env            []string
	Stdin          string
	MaxOutputBytes int64
	// StdoutLine is invoked as each complete stdout line arrives. It is used
	// for durable provider session events that must be saved before the child
	// exits. The complete stdout stream is still returned in ProcessResult.
	StdoutLine func([]byte) error
}

type ProcessResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	// ProcessStarted is true only after the OS accepted the child. A false
	// value is reliable evidence that no provider/helper process could run.
	// Injectable ProcessFunc implementations should set it when they start a
	// child before returning a setup error.
	ProcessStarted bool
}

// ProcessFunc is injectable for adapter tests. Production adapters use
// RunProcess, which drains both pipes concurrently.
type ProcessFunc func(context.Context, ProcessSpec) (ProcessResult, error)

// InteractiveProcessFunc is the terminal-attached process seam used by
// provider login commands.
type InteractiveProcessFunc func(context.Context, string, []string, string, []string, io.Reader, io.Writer, io.Writer) error

func RunInteractiveProcess(ctx context.Context, path string, args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

func RunProcess(ctx context.Context, spec ProcessSpec) (ProcessResult, error) {
	if spec.MaxOutputBytes <= 0 {
		spec.MaxOutputBytes = defaultMaxOutputBytes
	}
	cmd := exec.Command(spec.Path, spec.Args...)
	configureChildProcess(cmd)
	cmd.Dir, cmd.Env = spec.Dir, spec.Env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return ProcessResult{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return ProcessResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return ProcessResult{}, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return ProcessResult{ProcessStarted: false}, err
	}
	releaseChild, superviseErr := superviseChildProcess(cmd)
	if superviseErr != nil {
		forceKillChildProcess(cmd)
		_ = cmd.Wait()
		return ProcessResult{ProcessStarted: true}, superviseErr
	}
	processDone := make(chan struct{})
	var terminateOnce sync.Once
	var forceOnce sync.Once
	go func() {
		select {
		case <-ctx.Done():
			terminateOnce.Do(func() { terminateChildProcess(cmd) })
			graceTimer := time.NewTimer(processGracefulStopWindow)
			defer graceTimer.Stop()
			select {
			case <-processDone:
			case <-graceTimer.C:
				forceOnce.Do(func() { forceKillChildProcess(cmd) })
			}
		case <-processDone:
		}
	}()

	type readResult struct {
		data []byte
		err  error
	}
	outCh := make(chan readResult, 1)
	errCh := make(chan readResult, 1)
	stdinCh := make(chan error, 1)
	kill := func() {
		forceOnce.Do(func() { forceKillChildProcess(cmd) })
	}
	go func() {
		_, writeErr := io.WriteString(stdin, spec.Stdin)
		if closeErr := stdin.Close(); writeErr == nil {
			writeErr = closeErr
		}
		stdinCh <- writeErr
	}()
	// stdout and stderr share one budget. A noisy stderr stream must not let a
	// child consume twice the documented limit, and both pipes are drained at
	// the same time so neither can deadlock the other.
	var used atomic.Int64
	go func() {
		b, e := readBoundedSharedLines(stdout, spec.MaxOutputBytes, &used, kill, spec.StdoutLine)
		outCh <- readResult{b, e}
	}()
	go func() { b, e := readBoundedShared(stderr, spec.MaxOutputBytes, &used, kill); errCh <- readResult{b, e} }()
	out, serr, stdinErr := <-outCh, <-errCh, <-stdinCh
	if out.err != nil || serr.err != nil || stdinErr != nil {
		// Force-kill before Wait: an over-limit reader may have stopped
		// consuming a pipe while the child is blocked writing to it.
		kill()
	}
	waitErr := cmd.Wait()
	releaseChild()
	close(processDone)
	result := ProcessResult{
		Stdout:         out.data,
		Stderr:         serr.data,
		ExitCode:       exitCode(waitErr),
		ProcessStarted: true,
	}
	if out.err != nil {
		return result, out.err
	}
	if serr.err != nil {
		return result, serr.err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if waitErr != nil {
		message := strings.TrimSpace(string(result.Stderr))
		if message == "" {
			message = waitErr.Error()
		}
		return result, fmt.Errorf("provider process exited %d: %s", result.ExitCode, message)
	}
	if stdinErr != nil {
		return result, stdinErr
	}
	return result, nil
}

func readBoundedSharedLines(r io.Reader, limit int64, used *atomic.Int64, kill func(), line func([]byte) error) ([]byte, error) {
	if line == nil {
		return readBoundedShared(r, limit, used, kill)
	}
	if limit <= 0 {
		limit = defaultMaxOutputBytes
	}
	var result, pending []byte
	buffer := make([]byte, 32<<10)
	for {
		n, err := r.Read(buffer)
		if n > 0 {
			before := used.Add(int64(n)) - int64(n)
			remaining := limit - before
			if remaining <= 0 {
				kill()
				return result, ErrOutputLimit
			}
			keep := int64(n)
			if keep > remaining {
				keep = remaining
			}
			chunk := buffer[:int(keep)]
			result = append(result, chunk...)
			pending = append(pending, chunk...)
			for {
				newline := bytes.IndexByte(pending, '\n')
				if newline < 0 {
					break
				}
				current := bytes.TrimSuffix(pending[:newline], []byte{'\r'})
				if callErr := line(current); callErr != nil {
					kill()
					return result, callErr
				}
				pending = pending[newline+1:]
			}
			if keep != int64(n) {
				kill()
				return result, ErrOutputLimit
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(pending) > 0 {
					if callErr := line(bytes.TrimSuffix(pending, []byte{'\r'})); callErr != nil {
						return result, callErr
					}
				}
				return result, nil
			}
			return result, err
		}
	}
}

func readBounded(r io.Reader, limit int64, kill func()) ([]byte, error) {
	if limit <= 0 {
		limit = defaultMaxOutputBytes
	}
	reader := io.LimitReader(r, limit+1)
	b, err := io.ReadAll(reader)
	if int64(len(b)) > limit {
		kill()
		return b[:limit], ErrOutputLimit
	}
	return b, err
}

func readBoundedShared(r io.Reader, limit int64, used *atomic.Int64, kill func()) ([]byte, error) {
	if limit <= 0 {
		limit = defaultMaxOutputBytes
	}
	var result []byte
	buffer := make([]byte, 32<<10)
	for {
		n, err := r.Read(buffer)
		if n > 0 {
			before := used.Add(int64(n)) - int64(n)
			remaining := limit - before
			if remaining <= 0 {
				kill()
				return result, ErrOutputLimit
			}
			keep := int64(n)
			if keep > remaining {
				keep = remaining
			}
			result = append(result, buffer[:int(keep)]...)
			if keep != int64(n) {
				kill()
				return result, ErrOutputLimit
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return result, nil
			}
			return result, err
		}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
