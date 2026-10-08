//go:build linux

package mediaexec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
)

const (
	defaultPollInterval   = 5 * time.Millisecond
	defaultCleanupTimeout = time.Second
	cpuPeriodMicros       = int64(100000)
)

var requiredControllers = []string{"cpu", "memory", "pids"}

// NewCgroupRunner constructs the Linux production runner for one explicitly
// delegated cgroup-v2 subtree. It never discovers or uses a global cgroup root.
func NewCgroupRunner(root string) (Runner, Result) {
	canonical, ok := canonicalDelegatedRoot(root)
	if !ok {
		return nil, FailedResult(FailureContainmentSetup)
	}
	return newLinuxRunner(canonical, linuxRunnerDeps{}), SuccessfulResult()
}

func canonicalDelegatedRoot(root string) (string, bool) {
	if strings.IndexByte(root, 0) >= 0 || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return "", false
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || !filepath.IsAbs(canonical) || canonical == string(filepath.Separator) || canonical == "/sys/fs/cgroup" {
		return "", false
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", false
	}
	if _, err := os.Stat(filepath.Join(canonical, "cgroup.kill")); err != nil {
		return "", false
	}
	return canonical, true
}

type linuxRunner struct {
	root string
	deps linuxRunnerDeps
}

type linuxRunnerDeps struct {
	fs             cgroupFS
	launcher       processLauncher
	pollInterval   time.Duration
	cleanupTimeout time.Duration
	name           func() (string, error)
	newTimer       func(time.Duration) executionTimer
	now            func() time.Time
}

func newLinuxRunner(root string, deps linuxRunnerDeps) *linuxRunner {
	if deps.fs == nil {
		deps.fs = osCgroupFS{}
	}
	if deps.launcher == nil {
		deps.launcher = osProcessLauncher{}
	}
	if deps.pollInterval <= 0 {
		deps.pollInterval = defaultPollInterval
	}
	if deps.cleanupTimeout <= 0 {
		deps.cleanupTimeout = defaultCleanupTimeout
	}
	if deps.name == nil {
		deps.name = randomCgroupName
	}
	if deps.newTimer == nil {
		deps.newTimer = newRealExecutionTimer
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	return &linuxRunner{root: root, deps: deps}
}

func (runner *linuxRunner) Begin(ctx context.Context, limits media.Limits) (Execution, Result) {
	if ctx == nil || limits.Validate() != nil {
		return nil, FailedResult(FailureContainmentSetup)
	}
	if result := contextFailure(ctx); result.valid() {
		return nil, result
	}
	if !runner.deps.fs.Exists(filepath.Join(runner.root, "cgroup.kill")) {
		return nil, FailedResult(FailureContainmentUnavailable)
	}
	cgroupType, err := runner.deps.fs.ReadFile(filepath.Join(runner.root, "cgroup.type"))
	if err != nil || strings.TrimSpace(string(cgroupType)) != "domain" {
		return nil, FailedResult(FailureContainmentUnavailable)
	}
	controllers, err := runner.deps.fs.ReadFile(filepath.Join(runner.root, "cgroup.controllers"))
	if err != nil || !containsControllers(controllers, requiredControllers) {
		return nil, FailedResult(FailureContainmentUnavailable)
	}
	subtree := filepath.Join(runner.root, "cgroup.subtree_control")
	if _, err := runner.deps.fs.ReadFile(subtree); err != nil || runner.deps.fs.WriteFile(subtree, []byte("+cpu +memory +pids")) != nil {
		return nil, FailedResult(FailureContainmentUnavailable)
	}
	name, err := runner.deps.name()
	if err != nil || name == "" || filepath.Base(name) != name {
		return nil, FailedResult(FailureContainmentSetup)
	}
	parent := filepath.Join(runner.root, name)
	if err := runner.deps.fs.Mkdir(parent); err != nil {
		return nil, FailedResult(FailureContainmentSetup)
	}
	execution := &linuxExecution{ctx: ctx, limits: limits, parent: parent, deps: runner.deps}
	if result := execution.configure(); !result.succeeded() {
		if runner.deps.fs.Remove(parent) != nil {
			return nil, FailedResult(FailureContainmentCleanup)
		}
		return nil, result
	}
	return execution, SuccessfulResult()
}

func containsControllers(data []byte, required []string) bool {
	available := make(map[string]bool)
	for _, field := range strings.Fields(string(data)) {
		available[field] = true
	}
	for _, controller := range required {
		if !available[controller] {
			return false
		}
	}
	return true
}

func randomCgroupName() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "pepicanvas-media-" + hex.EncodeToString(value[:]), nil
}

type linuxExecution struct {
	mu       sync.Mutex
	ctx      context.Context
	limits   media.Limits
	parent   string
	deps     linuxRunnerDeps
	baseline uint64
	children []string
	command  uint64
	running  bool
	closed   bool
	active   startedProcess
}

func (execution *linuxExecution) configure() Result {
	if !execution.requireFiles("cgroup.kill", "cgroup.events", "cgroup.type", "cgroup.controllers", "cgroup.subtree_control", "pids.max", "memory.max", "memory.swap.max", "cpu.max", "cpu.stat", "memory.events", "pids.events") {
		return FailedResult(FailureContainmentUnavailable)
	}
	cgroupType, err := execution.deps.fs.ReadFile(filepath.Join(execution.parent, "cgroup.type"))
	if err != nil || strings.TrimSpace(string(cgroupType)) != "domain" {
		return FailedResult(FailureContainmentUnavailable)
	}
	controllers, err := execution.deps.fs.ReadFile(filepath.Join(execution.parent, "cgroup.controllers"))
	if err != nil || !containsControllers(controllers, requiredControllers) {
		return FailedResult(FailureContainmentUnavailable)
	}
	if err := execution.deps.fs.WriteFile(filepath.Join(execution.parent, "cgroup.kill"), []byte("1")); err != nil {
		return FailedResult(FailureContainmentUnavailable)
	}
	writes := []struct{ name, value string }{
		{"pids.max", strconv.Itoa(execution.limits.MaxTasks)},
		{"memory.max", strconv.FormatInt(execution.limits.MaxMemoryBytes, 10)},
		{"memory.swap.max", "0"},
		{"cpu.max", strconv.FormatInt(cpuPeriodMicros, 10) + " " + strconv.FormatInt(cpuPeriodMicros, 10)},
		{"cgroup.subtree_control", "+cpu +memory +pids"},
	}
	for _, write := range writes {
		if err := execution.deps.fs.WriteFile(filepath.Join(execution.parent, write.name), []byte(write.value)); err != nil {
			return FailedResult(FailureContainmentSetup)
		}
		if write.name != "cgroup.subtree_control" {
			actual, err := execution.deps.fs.ReadFile(filepath.Join(execution.parent, write.name))
			if err != nil || strings.TrimSpace(string(actual)) != write.value {
				return FailedResult(FailureContainmentSetup)
			}
		}
	}
	probe := filepath.Join(execution.parent, ".delegation-probe")
	if err := execution.deps.fs.Mkdir(probe); err != nil {
		return FailedResult(FailureContainmentUnavailable)
	}
	if err := execution.deps.fs.Remove(probe); err != nil {
		return FailedResult(FailureContainmentSetup)
	}
	baseline, err := execution.cpuUsage()
	if err != nil {
		return FailedResult(FailureContainmentSetup)
	}
	execution.baseline = baseline
	return SuccessfulResult()
}

func (execution *linuxExecution) requireFiles(names ...string) bool {
	for _, name := range names {
		if !execution.deps.fs.Exists(filepath.Join(execution.parent, name)) {
			return false
		}
	}
	return true
}

func (execution *linuxExecution) Run(command Command, stdout, stderr io.Writer) Result {
	execution.mu.Lock()
	if execution.closed || execution.running {
		execution.mu.Unlock()
		return FailedResult(FailureInternal)
	}
	execution.running = true
	execution.mu.Unlock()
	defer func() {
		execution.mu.Lock()
		execution.running = false
		execution.active = nil
		execution.mu.Unlock()
	}()

	if command.Path == "" || !filepath.IsAbs(command.Path) || command.Dir == "" || !filepath.IsAbs(command.Dir) || stdout == nil || stderr == nil {
		return FailedResult(FailureInternal)
	}
	if result := contextFailure(execution.ctx); result.valid() {
		return result
	}
	beforeMemory, err := execution.eventCounters("memory.events", "max", "oom", "oom_kill")
	if err != nil {
		return FailedResult(FailureInternal)
	}
	beforeTasks, err := execution.eventCounters("pids.events", "max")
	if err != nil {
		return FailedResult(FailureInternal)
	}

	execution.mu.Lock()
	execution.command++
	child := filepath.Join(execution.parent, "command-"+strconv.FormatUint(execution.command, 10))
	execution.mu.Unlock()
	if err := execution.deps.fs.Mkdir(child); err != nil {
		return FailedResult(FailureContainmentSetup)
	}
	execution.mu.Lock()
	execution.children = append(execution.children, child)
	execution.mu.Unlock()
	dir, err := execution.deps.fs.OpenDir(child)
	if err != nil {
		return FailedResult(FailureContainmentSetup)
	}
	defer dir.Close()

	spec := launchSpec{command: cloneCommand(command), cgroupPath: child, cgroupFD: int(dir.Fd()), useCgroupFD: true, setpgid: true}
	process, failure := execution.deps.launcher.Start(spec, stdout, stderr)
	if failure.unavailable {
		return FailedResult(FailureContainmentUnavailable)
	}
	if failure.internal || process == nil {
		return FailedResult(FailureInternal)
	}
	execution.mu.Lock()
	execution.active = process
	execution.mu.Unlock()

	waited := make(chan processResult, 1)
	go func() { waited <- process.Wait() }()
	var outcome processResult
	var forced Result
	for {
		timer := execution.deps.newTimer(execution.deps.pollInterval)
		select {
		case outcome = <-waited:
			timer.Stop()
			goto classified
		case <-execution.ctx.Done():
			timer.Stop()
			forced = contextFailure(execution.ctx)
			if !execution.killAndReap(process, waited, &outcome) {
				forced = FailedResult(FailureInternal)
			}
			goto classified
		case <-timer.Channel():
			usage, readErr := execution.cpuUsage()
			if readErr != nil || usage < execution.baseline {
				forced = FailedResult(FailureInternal)
				_ = execution.killAndReap(process, waited, &outcome)
				goto classified
			}
			if usage-execution.baseline >= uint64(execution.limits.MaxCPUTime/time.Microsecond) {
				forced = FailedResult(FailureCPULimit)
				if !execution.killAndReap(process, waited, &outcome) {
					forced = FailedResult(FailureInternal)
				}
				goto classified
			}
		}
	}

classified:
	if kind := execution.eventDelta(beforeMemory, beforeTasks); kind != 0 {
		return FailedResult(kind)
	}
	if forced.valid() {
		return forced
	}
	usage, err := execution.cpuUsage()
	if err != nil || usage < execution.baseline {
		return FailedResult(FailureInternal)
	}
	if usage-execution.baseline >= uint64(execution.limits.MaxCPUTime/time.Microsecond) {
		return FailedResult(FailureCPULimit)
	}
	if outcome.internal {
		return FailedResult(FailureInternal)
	}
	if outcome.exited {
		return FailedResult(FailureToolExit)
	}
	return SuccessfulResult()
}

func cloneCommand(command Command) Command {
	command.Args = append([]string(nil), command.Args...)
	command.Env = append([]string(nil), command.Env...)
	return command
}

func (execution *linuxExecution) killAndReap(process startedProcess, waited <-chan processResult, outcome *processResult) bool {
	err := execution.deps.fs.WriteFile(filepath.Join(execution.parent, "cgroup.kill"), []byte("1"))
	process.KillGroup()
	*outcome = <-waited
	return err == nil
}

func (execution *linuxExecution) eventDelta(memoryBefore, tasksBefore map[string]uint64) FailureKind {
	memoryAfter, err := execution.eventCounters("memory.events", "max", "oom", "oom_kill")
	if err != nil {
		return FailureInternal
	}
	for _, key := range []string{"max", "oom", "oom_kill"} {
		if memoryAfter[key] < memoryBefore[key] {
			return FailureInternal
		}
		if memoryAfter[key] > memoryBefore[key] {
			return FailureMemoryLimit
		}
	}
	tasksAfter, err := execution.eventCounters("pids.events", "max")
	if err != nil || tasksAfter["max"] < tasksBefore["max"] {
		return FailureInternal
	}
	if tasksAfter["max"] > tasksBefore["max"] {
		return FailureTaskLimit
	}
	return 0
}

func (execution *linuxExecution) cpuUsage() (uint64, error) {
	values, err := execution.eventCounters("cpu.stat", "usage_usec")
	return values["usage_usec"], err
}

func (execution *linuxExecution) eventCounters(name string, required ...string) (map[string]uint64, error) {
	data, err := execution.deps.fs.ReadFile(filepath.Join(execution.parent, name))
	if err != nil {
		return nil, err
	}
	values := make(map[string]uint64)
	fields := strings.Fields(string(data))
	if len(fields)%2 != 0 {
		return nil, errors.New("malformed cgroup counter")
	}
	for index := 0; index < len(fields); index += 2 {
		if _, duplicate := values[fields[index]]; duplicate {
			return nil, errors.New("duplicate cgroup counter")
		}
		value, parseErr := strconv.ParseUint(fields[index+1], 10, 64)
		if parseErr != nil {
			return nil, errors.New("malformed cgroup counter")
		}
		values[fields[index]] = value
	}
	for _, key := range required {
		if _, ok := values[key]; !ok {
			return nil, errors.New("missing cgroup counter")
		}
	}
	return values, nil
}

func (execution *linuxExecution) Close() Result {
	execution.mu.Lock()
	if execution.closed || execution.running {
		execution.mu.Unlock()
		return FailedResult(FailureInternal)
	}
	execution.closed = true
	children := append([]string(nil), execution.children...)
	execution.mu.Unlock()

	failed := execution.deps.fs.WriteFile(filepath.Join(execution.parent, "cgroup.kill"), []byte("1")) != nil
	deadline := execution.deps.now().Add(execution.deps.cleanupTimeout)
	for {
		populated, err := execution.populated()
		if err != nil {
			failed = true
			break
		}
		if !populated {
			break
		}
		if !execution.deps.now().Before(deadline) || !execution.deps.fs.Pause(context.Background(), execution.deps.pollInterval) {
			failed = true
			break
		}
	}
	for index := len(children) - 1; index >= 0; index-- {
		if err := execution.deps.fs.Remove(children[index]); err != nil {
			failed = true
		}
	}
	if err := execution.deps.fs.Remove(execution.parent); err != nil {
		failed = true
	}
	if failed {
		return FailedResult(FailureContainmentCleanup)
	}
	return SuccessfulResult()
}

func (execution *linuxExecution) populated() (bool, error) {
	values, err := execution.eventCounters("cgroup.events", "populated")
	if err != nil || values["populated"] > 1 {
		return false, errors.New("malformed populated state")
	}
	return values["populated"] == 1, nil
}

func contextFailure(ctx context.Context) Result {
	if ctx == nil || ctx.Err() == nil {
		return Result{}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return FailedResult(FailureTimeout)
	}
	return FailedResult(FailureCancelled)
}

type cgroupFS interface {
	ReadFile(string) ([]byte, error)
	WriteFile(string, []byte) error
	Exists(string) bool
	Mkdir(string) error
	OpenDir(string) (cgroupDir, error)
	Remove(string) error
	Pause(context.Context, time.Duration) bool
}

type cgroupDir interface {
	Fd() uintptr
	Close() error
}

type executionTimer interface {
	Channel() <-chan time.Time
	Stop() bool
}

type realExecutionTimer struct{ timer *time.Timer }

func newRealExecutionTimer(duration time.Duration) executionTimer {
	return &realExecutionTimer{timer: time.NewTimer(duration)}
}
func (timer *realExecutionTimer) Channel() <-chan time.Time { return timer.timer.C }
func (timer *realExecutionTimer) Stop() bool                { return timer.timer.Stop() }

type osCgroupFS struct{}

func (osCgroupFS) ReadFile(path string) ([]byte, error)     { return os.ReadFile(path) }
func (osCgroupFS) WriteFile(path string, data []byte) error { return os.WriteFile(path, data, 0) }
func (osCgroupFS) Exists(path string) bool                  { _, err := os.Stat(path); return err == nil }
func (osCgroupFS) Mkdir(path string) error                  { return os.Mkdir(path, 0o700) }
func (osCgroupFS) OpenDir(path string) (cgroupDir, error)   { return os.Open(path) }
func (osCgroupFS) Remove(path string) error                 { return os.Remove(path) }
func (osCgroupFS) Pause(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type launchSpec struct {
	command     Command
	cgroupPath  string
	cgroupFD    int
	useCgroupFD bool
	setpgid     bool
}

type launchFailure struct {
	unavailable bool
	internal    bool
}

type processResult struct {
	exited   bool
	internal bool
}

type processLauncher interface {
	Start(launchSpec, io.Writer, io.Writer) (startedProcess, launchFailure)
}

type startedProcess interface {
	Wait() processResult
	KillGroup()
}

type osProcessLauncher struct{}

func (osProcessLauncher) Start(spec launchSpec, stdout, stderr io.Writer) (startedProcess, launchFailure) {
	command := exec.Command(spec.command.Path, spec.command.Args...)
	command.Env = append([]string(nil), spec.command.Env...)
	command.Dir = spec.command.Dir
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = linuxSysProcAttr(spec.cgroupFD)
	if err := command.Start(); err != nil {
		if atomicLaunchUnavailable(err) {
			return nil, launchFailure{unavailable: true}
		}
		return nil, launchFailure{internal: true}
	}
	return &osStartedProcess{command: command}, launchFailure{}
}

func linuxSysProcAttr(cgroupFD int) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: cgroupFD, Setpgid: true}
}

func atomicLaunchUnavailable(err error) bool {
	return errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EOPNOTSUPP)
}

type osStartedProcess struct {
	command *exec.Cmd
}

func (process *osStartedProcess) Wait() processResult {
	err := process.command.Wait()
	if err == nil {
		return processResult{}
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return processResult{exited: true}
	}
	return processResult{internal: true}
}

func (process *osStartedProcess) KillGroup() {
	if process.command.Process != nil {
		_ = syscall.Kill(-process.command.Process.Pid, syscall.SIGKILL)
	}
}
