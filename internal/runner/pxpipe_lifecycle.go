package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	pxpipeRoutePattern       = codexChatGPTHost + "/backend-api/codex/responses*"
	pxpipeClaudeRoutePattern = claudeAPIHost + "/v1/messages*"
	pxpipeServerHost         = "127.0.0.1"
	pxpipeStartupTimeout     = 6 * time.Second
	pxpipeShutdownTimeout    = 2 * time.Second
	pxpipePortAttempts       = 8
	pxpipeCapabilityTimeout  = 2 * time.Second
	pxpipeReadinessPoll      = 100 * time.Millisecond
)

// PXPipeLaunchSpec is the boundary between provider transport preparation and
// the helper lifecycle. Provider fields are copied into the private server
// environment; the helper never receives RoleMux's durable runtime snapshot.
type PXPipeLaunchSpec struct {
	PXPipePath         string
	Provider           ProcessSpec
	ProviderName       string
	ServerEnv          []string
	EventsFile         string
	RoutePrefix        string
	TaskStartsOnLaunch bool
	Diagnostic         func(string)
}

// PXPipeLauncher changes only a provider task launch. It is intentionally
// separate from ProcessFunc so auth, version, login, discovery, and selection
// probes cannot accidentally pass through pxpipe.
type PXPipeLauncher interface {
	Launch(context.Context, PXPipeLaunchSpec) (ProcessResult, error)
}

// CodexTaskLauncher remains an alias for compatibility with the Codex adapter
// tests while Claude and future providers share the generic lifecycle.
type CodexTaskLauncher = PXPipeLauncher

// PXPipeLaunchError records whether a helper failure happened before the
// provider could have accepted the task. Only that phase is eligible for a
// direct fallback; after the boundary, replaying the prompt could duplicate
// work.
type PXPipeLaunchError struct {
	BeforeTask bool
	Cause      error
}

func (e *PXPipeLaunchError) Error() string {
	if e == nil || e.Cause == nil {
		return "pxpipe launch failed"
	}
	return e.Cause.Error()
}

func (e *PXPipeLaunchError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// PXPipeServerSpec and PXPipeServer are injectable seams for lifecycle tests.
// The production server requires its owned child to announce the configured
// address and then pass an HTTP readiness check before it is accepted.
type PXPipeServerSpec struct {
	Path           string
	Env            []string
	Dir            string
	Port           int
	StartupTimeout time.Duration
	Readiness      func(context.Context, string) error
}

type PXPipeServer interface {
	WaitReady(context.Context) error
	Done() <-chan struct{}
	Err() error
	Stop(context.Context) error
}

type PXPipeServerFactory func(context.Context, PXPipeServerSpec) (PXPipeServer, error)
type PXPipePortChooser func(context.Context) ([]int, error)
type PXPipeCapabilityCheck func(context.Context, ProcessSpec) error

// PXPipeTaskLauncher owns one foreground server for one provider turn. It never
// talks to, reconfigures, or stops an existing user daemon.
type PXPipeTaskLauncher struct {
	Path            string
	Process         ProcessFunc
	ServerFactory   PXPipeServerFactory
	PortChooser     PXPipePortChooser
	CapabilityCheck PXPipeCapabilityCheck
	MaxAttempts     int
	StartupTimeout  time.Duration
	ShutdownTimeout time.Duration
}

func (p *PXPipeTaskLauncher) Launch(ctx context.Context, spec PXPipeLaunchSpec) (ProcessResult, error) {
	if err := ctx.Err(); err != nil {
		return ProcessResult{}, err
	}
	path := strings.TrimSpace(spec.PXPipePath)
	if path == "" && p != nil {
		path = strings.TrimSpace(p.Path)
	}
	usesProductionFactory := p == nil || p.ServerFactory == nil
	if path == "" || (usesProductionFactory && !executableFile(path)) {
		return ProcessResult{}, &PXPipeLaunchError{BeforeTask: true, Cause: errors.New("pxpipe executable is unavailable")}
	}
	if spec.Provider.Path == "" {
		return ProcessResult{}, &PXPipeLaunchError{BeforeTask: true, Cause: errors.New("cannot wrap an empty provider executable")}
	}
	process := RunProcess
	if p != nil && p.Process != nil {
		process = p.Process
	}
	factory := defaultPXPipeServerFactory
	if p != nil && p.ServerFactory != nil {
		factory = p.ServerFactory
	}
	startupTimeout := pxpipeStartupTimeout
	shutdownTimeout := pxpipeShutdownTimeout
	if p != nil {
		if p.StartupTimeout > 0 {
			startupTimeout = p.StartupTimeout
		}
		if p.ShutdownTimeout > 0 {
			shutdownTimeout = p.ShutdownTimeout
		}
	}
	serverEnv := append([]string(nil), spec.ServerEnv...)
	if len(serverEnv) == 0 {
		serverEnv = PXPipeServerEnvironment(spec.Provider.Env)
	}
	eventsFile := spec.EventsFile
	if eventsFile == "" {
		eventsFile = pxpipeEventsFile(serverEnv)
	}
	if eventsFile != "" && !filepath.IsAbs(eventsFile) && spec.Provider.Dir != "" {
		eventsFile = filepath.Join(spec.Provider.Dir, eventsFile)
	}
	eventsOffset := pxpipeEventsOffset(eventsFile)
	capabilityCheck := checkPXPipeWarpCapability
	if p != nil && p.CapabilityCheck != nil {
		capabilityCheck = p.CapabilityCheck
	}
	capabilityCtx, cancelCapability := context.WithTimeout(ctx, pxpipeCapabilityTimeout)
	capabilityErr := capabilityCheck(capabilityCtx, ProcessSpec{
		Path: path, Args: []string{"warp", "--help"}, Dir: spec.Provider.Dir,
		Env: serverEnv, MaxOutputBytes: 64 << 10,
	})
	capabilityExpired := capabilityCtx.Err() != nil
	cancelCapability()
	if capabilityErr != nil || capabilityExpired {
		if ctx.Err() != nil {
			return ProcessResult{}, ctx.Err()
		}
		if capabilityErr == nil {
			capabilityErr = errors.New("pxpipe warp capability probe timed out")
		}
		return ProcessResult{}, &PXPipeLaunchError{BeforeTask: true, Cause: capabilityErr}
	}
	ports, err := choosePXPipePorts(ctx, p)
	if err != nil {
		if ctx.Err() != nil {
			return ProcessResult{}, ctx.Err()
		}
		return ProcessResult{}, &PXPipeLaunchError{BeforeTask: true, Cause: err}
	}
	maxAttempts := pxpipePortAttempts
	if p != nil && p.MaxAttempts > 0 && p.MaxAttempts < maxAttempts {
		maxAttempts = p.MaxAttempts
	}
	if len(ports) < maxAttempts {
		maxAttempts = len(ports)
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return ProcessResult{}, err
		}
		port := ports[attempt]
		startCtx, cancelStart := context.WithTimeout(ctx, startupTimeout)
		server, startErr := factory(startCtx, PXPipeServerSpec{
			Path: path, Env: pxpipeEnvironmentWithPort(serverEnv, port), Dir: spec.Provider.Dir, Port: port,
			StartupTimeout: startupTimeout, Readiness: defaultPXPipeReadiness,
		})
		startExpired := startCtx.Err() != nil
		cancelStart()
		if startErr != nil {
			if server != nil {
				stopPXPipeServer(server, shutdownTimeout)
			}
			if ctx.Err() != nil {
				return ProcessResult{}, ctx.Err()
			}
			lastErr = startErr
			continue
		}
		if server == nil {
			lastErr = errors.New("pxpipe server factory returned no server")
			continue
		}
		if startExpired {
			stopPXPipeServer(server, shutdownTimeout)
			if ctx.Err() != nil {
				return ProcessResult{}, ctx.Err()
			}
			lastErr = errors.New("pxpipe server startup timed out")
			continue
		}
		readyCtx, cancelReady := context.WithTimeout(ctx, startupTimeout)
		readyErr := server.WaitReady(readyCtx)
		cancelReady()
		if readyErr != nil {
			stopPXPipeServer(server, shutdownTimeout)
			if ctx.Err() != nil {
				return ProcessResult{}, ctx.Err()
			}
			lastErr = readyErr
			continue
		}
		select {
		case <-server.Done():
			stopPXPipeServer(server, shutdownTimeout)
			lastErr = server.Err()
			if lastErr == nil {
				lastErr = errors.New("pxpipe server exited before the provider task started")
			}
			continue
		default:
		}
		if spec.Diagnostic != nil {
			spec.Diagnostic("pxpipe dashboard (this turn): http://" + pxpipeServerHost + ":" + strconv.Itoa(port) + "/; events: " + eventsFile)
		}
		route := spec.RoutePrefix
		if route == "" {
			route = pxpipeRoutePattern
		}
		warpArgs := []string{"warp", "--route", route + "=http://" + pxpipeServerHost + ":" + strconv.Itoa(port), "--", spec.Provider.Path}
		warpArgs = append(warpArgs, spec.Provider.Args...)
		warpSpec := spec.Provider
		warpSpec.Path, warpSpec.Args = path, warpArgs
		var taskStarted atomic.Bool
		providerLine := warpSpec.StdoutLine
		warpSpec.StdoutLine = func(line []byte) error {
			if sessionID, _ := codexSessionFromLine(line); sessionID != "" {
				taskStarted.Store(true)
			}
			if providerLine != nil {
				return providerLine(line)
			}
			return nil
		}
		taskCtx, cancelTask := context.WithCancel(ctx)
		resultCh := make(chan struct {
			result ProcessResult
			err    error
		}, 1)
		go func() {
			// Claude only emits its structured JSON after the turn completes, so
			// process acceptance is its conservative no-replay boundary. Codex
			// leaves this false and marks the boundary on thread.started instead.
			if spec.TaskStartsOnLaunch {
				taskStarted.Store(true)
			}
			result, runErr := process(taskCtx, warpSpec)
			if runErr != nil {
				runErr = annotatePXPipeStatus(runErr, eventsFile, eventsOffset)
				runErr = classifyPXPipeTaskError(taskStarted.Load(), result, runErr)
			}
			resultCh <- struct {
				result ProcessResult
				err    error
			}{result: result, err: runErr}
		}()
		select {
		case outcome := <-resultCh:
			cancelTask()
			stopPXPipeServer(server, shutdownTimeout)
			return outcome.result, outcome.err
		case <-server.Done():
			cancelTask()
			outcome := waitPXPipeTask(resultCh, shutdownTimeout)
			stopPXPipeServer(server, shutdownTimeout)
			cause := server.Err()
			if cause == nil {
				cause = errors.New("pxpipe server exited during the provider task")
			}
			if outcome.err != nil && !errors.Is(outcome.err, context.Canceled) {
				providerName := strings.TrimSpace(spec.ProviderName)
				if providerName == "" {
					providerName = "provider"
				}
				cause = fmt.Errorf("%w; %s task: %v", cause, providerName, outcome.err)
			}
			cause = annotatePXPipeStatus(cause, eventsFile, eventsOffset)
			return outcome.result, classifyPXPipeTaskError(taskStarted.Load(), outcome.result, cause)
		case <-ctx.Done():
			cancelTask()
			outcome := waitPXPipeTask(resultCh, shutdownTimeout)
			stopPXPipeServer(server, shutdownTimeout)
			if outcome.err != nil && !errors.Is(outcome.err, context.Canceled) {
				return outcome.result, outcome.err
			}
			return outcome.result, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("pxpipe could not start a private server")
	}
	return ProcessResult{}, &PXPipeLaunchError{BeforeTask: true, Cause: lastErr}
}

func pxpipeEventsOffset(path string) int64 {
	if strings.TrimSpace(path) == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	return info.Size()
}

func annotatePXPipeStatus(err error, path string, offset int64) error {
	if err == nil || strings.TrimSpace(path) == "" || strings.Contains(err.Error(), "pxpipe upstream status ") {
		return err
	}
	file, openErr := os.Open(path)
	if openErr != nil {
		return err
	}
	defer file.Close()
	if offset > 0 {
		if _, seekErr := file.Seek(offset, io.SeekStart); seekErr != nil {
			return err
		}
	}
	scanner := bufio.NewScanner(io.LimitReader(file, 1<<20))
	scanner.Buffer(make([]byte, 4096), 256<<10)
	status := 0
	for scanner.Scan() {
		var event struct {
			Status int `json:"status"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Status >= 400 {
			status = event.Status
		}
	}
	if status == 0 {
		return err
	}
	return fmt.Errorf("pxpipe upstream status %d: %w", status, err)
}

func classifyPXPipeTaskError(taskStarted bool, result ProcessResult, err error) error {
	if err == nil {
		return nil
	}
	// Codex marks this boundary with thread.started; providers without an early
	// durable event use process acceptance. Until the selected boundary is
	// observed, retrying the untouched command cannot duplicate a provider turn.
	if !result.ProcessStarted || !taskStarted {
		return &PXPipeLaunchError{BeforeTask: true, Cause: err}
	}
	return &PXPipeLaunchError{BeforeTask: false, Cause: err}
}

func checkPXPipeWarpCapability(ctx context.Context, spec ProcessSpec) error {
	result, err := RunProcess(ctx, spec)
	if err != nil {
		return errors.New("pxpipe warp capability probe failed")
	}
	output := strings.ToLower(string(append(append([]byte(nil), result.Stdout...), result.Stderr...)))
	if !strings.Contains(output, "warp") || !strings.Contains(output, "--route") || !strings.Contains(output, "-- cmd") {
		return errors.New("pxpipe does not advertise warp route support")
	}
	return nil
}

func choosePXPipePorts(ctx context.Context, launcher *PXPipeTaskLauncher) ([]int, error) {
	if launcher != nil && launcher.PortChooser != nil {
		return launcher.PortChooser(ctx)
	}
	return defaultPXPipePortChooser(ctx)
}

func defaultPXPipePortChooser(ctx context.Context) ([]int, error) {
	ports := make([]int, 0, pxpipePortAttempts)
	for len(ports) < pxpipePortAttempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		listener, err := net.Listen("tcp", pxpipeServerHost+":0")
		if err != nil {
			return nil, fmt.Errorf("choose a private pxpipe port: %w", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		ports = append(ports, port)
	}
	return ports, nil
}

func waitPXPipeTask(resultCh <-chan struct {
	result ProcessResult
	err    error
}, timeout time.Duration) (outcome struct {
	result ProcessResult
	err    error
}) {
	_ = timeout // RunProcess owns bounded TERM→KILL escalation and reaping.
	return <-resultCh
}

func stopPXPipeServer(server PXPipeServer, timeout time.Duration) {
	if server == nil {
		return
	}
	if timeout <= 0 {
		timeout = pxpipeShutdownTimeout
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// Stop owns both phases: TERM until the context deadline, then a forced
	// tree kill followed by cmd.Wait. Calling it synchronously ensures Launch
	// never returns while its private helper remains unreaped.
	_ = server.Stop(stopCtx)
}

type localPXPipeServer struct {
	cmd       *exec.Cmd
	done      chan struct{}
	lines     chan string
	errMu     sync.Mutex
	err       error
	stopOnce  sync.Once
	forceOnce sync.Once
	readiness func(context.Context, string) error
	port      int
	timeout   time.Duration
}

func defaultPXPipeServerFactory(ctx context.Context, spec PXPipeServerSpec) (PXPipeServer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Path == "" || spec.Port <= 0 {
		return nil, errors.New("invalid pxpipe server specification")
	}
	cmd := exec.Command(spec.Path)
	cmd.Dir, cmd.Env = spec.Dir, append([]string(nil), spec.Env...)
	configureChildProcess(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, err
	}
	releaseChild, superviseErr := superviseChildProcess(cmd)
	if superviseErr != nil {
		forceKillChildProcess(cmd)
		_ = cmd.Wait()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, superviseErr
	}
	server := &localPXPipeServer{
		cmd: cmd, done: make(chan struct{}), lines: make(chan string, 128), port: spec.Port,
		readiness: spec.Readiness, timeout: spec.StartupTimeout,
	}
	if server.readiness == nil {
		server.readiness = defaultPXPipeReadiness
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); scanPXPipeOutput(stdout, server.lines) }()
	go func() { defer readers.Done(); scanPXPipeOutput(stderr, server.lines) }()
	go func() {
		readers.Wait()
		close(server.lines)
	}()
	go func() {
		err := cmd.Wait()
		releaseChild()
		server.errMu.Lock()
		server.err = err
		server.errMu.Unlock()
		close(server.done)
	}()
	return server, nil
}

func scanPXPipeOutput(reader io.Reader, lines chan<- string) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	for scanner.Scan() {
		select {
		case lines <- scanner.Text():
		default:
			// Once readiness has been observed, logs are measurement noise. Do
			// not let an unbounded helper log stream block process reaping.
		}
	}
}

func (s *localPXPipeServer) WaitReady(ctx context.Context) error {
	if s == nil {
		return errors.New("nil pxpipe server")
	}
	deadline := s.timeout
	if deadline <= 0 {
		deadline = pxpipeStartupTimeout
	}
	readyTimer := time.NewTimer(deadline)
	defer readyTimer.Stop()
	announced := false
	lines := s.lines
	address := "http://" + pxpipeServerHost + ":" + strconv.Itoa(s.port)
	var readinessTicker *time.Ticker
	var readinessTick <-chan time.Time
	readiness := s.readiness
	if readiness == nil {
		readiness = defaultPXPipeReadiness
	}
	defer func() {
		if readinessTicker != nil {
			readinessTicker.Stop()
		}
	}()
	probeReadiness := func() bool {
		probeCtx, cancel := context.WithTimeout(ctx, 350*time.Millisecond)
		probeErr := readiness(probeCtx, address)
		cancel()
		if probeErr != nil {
			return false
		}
		select {
		case <-s.done:
			return false
		default:
			return true
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-readyTimer.C:
			return errors.New("pxpipe server readiness timed out")
		case <-s.done:
			if s.Err() != nil {
				return fmt.Errorf("pxpipe server exited before readiness: %v", s.Err())
			}
			return errors.New("pxpipe server exited before readiness")
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			if pxpipeListeningLine(line, s.port) {
				announced = true
				if readinessTicker == nil {
					readinessTicker = time.NewTicker(pxpipeReadinessPoll)
					readinessTick = readinessTicker.C
				}
			}
			if announced && probeReadiness() {
				return nil
			}
		case <-readinessTick:
			if announced && probeReadiness() {
				return nil
			}
		}
	}
}

func pxpipeListeningLine(line string, port int) bool {
	want := "[pxpipe] listening on http://" + pxpipeServerHost + ":" + strconv.Itoa(port)
	return strings.TrimSpace(line) == want
}

func (s *localPXPipeServer) Done() <-chan struct{} { return s.done }

func (s *localPXPipeServer) Err() error {
	if s == nil {
		return errors.New("nil pxpipe server")
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *localPXPipeServer) Stop(ctx context.Context) error {
	if s == nil || s.cmd == nil {
		return nil
	}
	select {
	case <-s.done:
		return nil
	default:
	}
	s.stopOnce.Do(func() { terminateChildProcess(s.cmd) })
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		s.forceOnce.Do(func() { forceKillChildProcess(s.cmd) })
		<-s.done
		return ctx.Err()
	}
}

func defaultPXPipeReadiness(ctx context.Context, address string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/proxy-stats", nil)
	if err != nil {
		return err
	}
	// Readiness must probe the owned loopback listener directly. Inherited
	// HTTP(S)_PROXY settings must not make an unrelated proxy look ready.
	client := &http.Client{
		Timeout:       350 * time.Millisecond,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("pxpipe readiness returned HTTP %d", response.StatusCode)
	}
	return nil
}

// PXPipeServerEnvironment builds a private Codex pxpipe-server environment.
func PXPipeServerEnvironment(providerEnv []string) []string {
	return pxpipeServerEnvironment(providerEnv, "OPENAI_UPSTREAM", codexChatGPTOrigin)
}

// ClaudePXPipeServerEnvironment builds a private Claude pxpipe-server
// environment. Provider credentials stay in the Claude child and are carried
// by the intercepted request; they are not copied into the helper process.
func ClaudePXPipeServerEnvironment(providerEnv []string) []string {
	return pxpipeServerEnvironment(providerEnv, "ANTHROPIC_UPSTREAM", claudeAPIBaseURL)
}

func pxpipeServerEnvironment(providerEnv []string, upstreamKey, upstream string) []string {
	remove := map[string]bool{
		"HOST": true, "PORT": true,
		"OPENAI_API_KEY": true, "OPENAI_BASE_URL": true, "OPENAI_API_BASE": true,
		"OPENAI_UPSTREAM": true, "OPENAI_MODELS": true,
		"CODEX_BASE_URL": true, "CODEX_API_BASE": true,
		"PXPIPE_UPSTREAM": true, "PXPIPE_PROVIDER": true, "PXPIPE_GATEWAY_BASE_URL": true, "PXPIPE_GATEWAY_HEADERS": true,
		"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true, "CLAUDE_CODE_OAUTH_TOKEN": true,
		"ANTHROPIC_UPSTREAM": true, "ANTHROPIC_BASE_URL": true, "ANTHROPIC_API_URL": true,
		"CLOUDFLARE_ACCOUNT_ID": true, "CLOUDFLARE_API_TOKEN": true, "CLOUDFLARE_MODELS": true,
	}
	values := map[string]string{}
	for _, item := range providerEnv {
		key, value, ok := strings.Cut(item, "=")
		if ok && key != "" && !remove[strings.ToUpper(key)] {
			values[key] = value
		}
	}
	values["HOST"] = pxpipeServerHost
	values[upstreamKey] = upstream
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	// runtimeEnvironment already sorts provider environments, but this helper
	// is public and should be deterministic on its own.
	sortStrings(result)
	return result
}

func pxpipeEnvironmentWithPort(environ []string, port int) []string {
	result := make([]string, 0, len(environ)+1)
	for _, item := range environ {
		key, _, ok := strings.Cut(item, "=")
		if ok && strings.EqualFold(key, "PORT") {
			continue
		}
		result = append(result, item)
	}
	result = append(result, "PORT="+strconv.Itoa(port))
	sortStrings(result)
	return result
}

func pxpipeEventsFile(environ []string) string {
	if value := environmentValue(environ, "PXPIPE_LOG"); value != "" {
		return value
	}
	home := environmentValue(environ, "HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return filepath.Join(".pxpipe", "events.jsonl")
	}
	return filepath.Join(home, ".pxpipe", "events.jsonl")
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
