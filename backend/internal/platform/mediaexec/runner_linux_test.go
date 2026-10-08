//go:build linux

package mediaexec

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
)

func TestNewCgroupRunnerRejectsUnsafeDelegatedRoots(t *testing.T) {
	t.Parallel()

	for _, root := range []string{"", "relative", "/", "/sys/fs/cgroup", "/tmp/../tmp/delegated", "/tmp/delegated\x00suffix"} {
		t.Run(root, func(t *testing.T) {
			runner, result := NewCgroupRunner(root)
			if runner != nil || result.failureKind() != FailureContainmentSetup {
				t.Fatalf("NewCgroupRunner(%q) = %#v, %#v; want nil setup failure", root, runner, result)
			}
		})
	}
}

func TestLinuxBeginValidatesDelegationAndConfiguresParentBeforeRun(t *testing.T) {
	t.Parallel()

	fs := newFakeCgroupFS("/delegated")
	runner := newLinuxRunner("/delegated", linuxRunnerDeps{fs: fs, launcher: &fakeLauncher{}})
	execution, result := runner.Begin(context.Background(), media.DefaultLimits())
	if !result.succeeded() || execution == nil {
		t.Fatalf("Begin() = %#v, %#v, want execution and success", execution, result)
	}

	writes := fs.writeLog()
	wantSuffixes := []string{
		"/delegated/cgroup.subtree_control=+cpu +memory +pids",
		"/pids.max=64",
		"/memory.max=536870912",
		"/memory.swap.max=0",
		"/cpu.max=100000 100000",
		"/cgroup.subtree_control=+cpu +memory +pids",
	}
	for _, want := range wantSuffixes {
		if !containsSuffix(writes, want) {
			t.Fatalf("writes = %#v, missing suffix %q", writes, want)
		}
	}
	if got := fs.firstMkdirIndex(); got < 0 || got > fs.firstLimitWriteIndex() {
		t.Fatalf("operation order = %#v, parent must exist before limits", fs.operations())
	}
}

func TestLinuxBeginFailsClosedAndRollsBackOnMissingCapability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*fakeCgroupFS)
	}{
		{name: "global root without cgroup kill", mutate: func(fs *fakeCgroupFS) { delete(fs.files, "/delegated/cgroup.kill") }},
		{name: "non-domain root", mutate: func(fs *fakeCgroupFS) { fs.files["/delegated/cgroup.type"] = "threaded\n" }},
		{name: "controller", mutate: func(fs *fakeCgroupFS) { fs.files["/delegated/cgroup.controllers"] = "cpu memory\n" }},
		{name: "kill", mutate: func(fs *fakeCgroupFS) { fs.omitOnCreate["cgroup.kill"] = true }},
		{name: "swap", mutate: func(fs *fakeCgroupFS) { fs.omitOnCreate["memory.swap.max"] = true }},
		{name: "malformed CPU stat", mutate: func(fs *fakeCgroupFS) { fs.createdDefaults["cpu.stat"] = "usage_usec nope\n" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeCgroupFS("/delegated")
			tt.mutate(fs)
			runner := newLinuxRunner("/delegated", linuxRunnerDeps{fs: fs, launcher: &fakeLauncher{}})
			execution, result := runner.Begin(context.Background(), media.DefaultLimits())
			if execution != nil || result.succeeded() {
				t.Fatalf("Begin() = %#v, %#v, want fail closed", execution, result)
			}
			if fs.ownedDirectoryCount() != 0 {
				t.Fatalf("owned directories remain after failed setup: %#v", fs.directories())
			}
			if (tt.name == "global root without cgroup kill" || tt.name == "non-domain root") && len(fs.writeLog()) != 0 {
				t.Fatalf("unsafe root was mutated: %#v", fs.writeLog())
			}
		})
	}
}

func TestLinuxRunUsesAtomicCgroupLaunchAndExactCommand(t *testing.T) {
	t.Parallel()

	fs := newFakeCgroupFS("/delegated")
	launcher := &fakeLauncher{processes: []*fakeProcess{{result: processResult{}}}}
	execution := beginFakeExecution(t, fs, launcher, media.DefaultLimits())
	command := Command{Path: "/trusted/tool", Args: []string{"--literal", "$(private)"}, Env: []string{"A=B"}, Dir: "/trusted/work"}
	if result := execution.Run(command, io.Discard, io.Discard); !result.succeeded() {
		t.Fatalf("Run() = %#v, want success", result)
	}
	if len(launcher.specs) != 1 {
		t.Fatalf("launches = %d, want 1", len(launcher.specs))
	}
	spec := launcher.specs[0]
	if !reflect.DeepEqual(spec.command, command) || !spec.useCgroupFD || spec.cgroupFD <= 0 || !spec.setpgid {
		t.Fatalf("launch spec = %#v, want exact command and atomic cgroup/process-group fields", spec)
	}
	for _, write := range fs.writeLog() {
		if strings.HasSuffix(strings.SplitN(write, "=", 2)[0], "/cgroup.procs") {
			t.Fatalf("post-start PID attachment observed: %q", write)
		}
	}
}

func TestLinuxRunSharesCPUUsageAndClassifiesEventDeltasBeforeExit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		limits    func() media.Limits
		before    map[string]string
		after     map[string]string
		processes []*fakeProcess
		want      FailureKind
	}{
		{
			name: "CPU is shared across commands",
			limits: func() media.Limits {
				limits := media.DefaultLimits()
				limits.MaxCPUTime = 10 * time.Microsecond
				return limits
			},
			after:     map[string]string{"cpu.stat": "usage_usec 11\n"},
			processes: []*fakeProcess{{result: processResult{}}, {block: true}},
			want:      FailureCPULimit,
		},
		{
			name:      "memory event precedes ordinary exit",
			limits:    media.DefaultLimits,
			before:    map[string]string{"memory.events": "max 0\noom 0\noom_kill 0\n"},
			after:     map[string]string{"memory.events": "max 1\noom 0\noom_kill 0\n"},
			processes: []*fakeProcess{{result: processResult{exited: true}}},
			want:      FailureMemoryLimit,
		},
		{
			name:      "task event precedes ordinary exit",
			limits:    media.DefaultLimits,
			before:    map[string]string{"pids.events": "max 0\n"},
			after:     map[string]string{"pids.events": "max 1\n"},
			processes: []*fakeProcess{{result: processResult{exited: true}}},
			want:      FailureTaskLimit,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeCgroupFS("/delegated")
			for name, value := range tt.before {
				fs.createdDefaults[name] = value
			}
			starts := 0
			launcher := &fakeLauncher{processes: tt.processes, afterStart: func(parent string) {
				starts++
				if len(tt.processes) == 2 && starts == 1 {
					return
				}
				for name, value := range tt.after {
					fs.set(filepath.Join(parent, name), value)
				}
			}}
			execution := beginFakeExecution(t, fs, launcher, tt.limits())
			if len(tt.processes) == 2 {
				if result := execution.Run(testCommand(), io.Discard, io.Discard); !result.succeeded() {
					t.Fatalf("first Run() = %#v", result)
				}
			}
			result := execution.Run(testCommand(), io.Discard, io.Discard)
			if result.failureKind() != tt.want {
				t.Fatalf("Run() = %#v, want %v", result, tt.want)
			}
		})
	}
}

func TestLinuxRunKillsBeforeReapAndRejectsConcurrentRun(t *testing.T) {
	t.Parallel()

	fs := newFakeCgroupFS("/delegated")
	process := &fakeProcess{block: true}
	process.initialize()
	launcher := &fakeLauncher{processes: []*fakeProcess{process}}
	ctx, cancel := context.WithCancel(context.Background())
	execution := beginFakeExecutionContext(t, ctx, fs, launcher, media.DefaultLimits())
	resultCh := make(chan Result, 1)
	go func() { resultCh <- execution.Run(testCommand(), io.Discard, io.Discard) }()
	<-process.started
	if result := execution.Run(testCommand(), io.Discard, io.Discard); result.failureKind() != FailureInternal {
		t.Fatalf("concurrent Run() = %#v, want internal failure", result)
	}
	cancel()
	result := <-resultCh
	if result.failureKind() != FailureCancelled {
		t.Fatalf("cancelled Run() = %#v, want cancellation", result)
	}
	if !fs.killBefore(process.waitObserved) {
		t.Fatalf("operations = %#v, want cgroup.kill before reap", fs.operations())
	}
}

func TestLinuxRunFailsClosedForDeadlineMalformedCountersAndAtomicLaunchFailure(t *testing.T) {
	t.Parallel()

	t.Run("deadline", func(t *testing.T) {
		fs := newFakeCgroupFS("/delegated")
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		execution := beginFakeExecutionContext(t, ctx, fs, &fakeLauncher{processes: []*fakeProcess{{block: true}}}, media.DefaultLimits())
		if result := execution.Run(testCommand(), io.Discard, io.Discard); result.failureKind() != FailureTimeout {
			t.Fatalf("Run() = %#v, want timeout", result)
		}
	})

	t.Run("malformed event counter", func(t *testing.T) {
		fs := newFakeCgroupFS("/delegated")
		fs.createdDefaults["memory.events"] = "max private\n"
		execution := beginFakeExecution(t, fs, &fakeLauncher{}, media.DefaultLimits())
		if result := execution.Run(testCommand(), io.Discard, io.Discard); result.failureKind() != FailureInternal {
			t.Fatalf("Run() = %#v, want internal failure", result)
		}
	})

	t.Run("atomic launch unavailable", func(t *testing.T) {
		fs := newFakeCgroupFS("/delegated")
		execution := beginFakeExecution(t, fs, &fakeLauncher{failure: launchFailure{unavailable: true}}, media.DefaultLimits())
		if result := execution.Run(testCommand(), io.Discard, io.Discard); result.failureKind() != FailureContainmentUnavailable {
			t.Fatalf("Run() = %#v, want unavailable", result)
		}
	})
}

func TestLinuxCloseWaitsForUnpopulatedAndRemovesOwnedOnlyDeepestFirst(t *testing.T) {
	t.Parallel()

	fs := newFakeCgroupFS("/delegated")
	launcher := &fakeLauncher{processes: []*fakeProcess{{result: processResult{}}}}
	execution := beginFakeExecution(t, fs, launcher, media.DefaultLimits())
	if result := execution.Run(testCommand(), io.Discard, io.Discard); !result.succeeded() {
		t.Fatal(result)
	}
	parent := fs.ownedParent()
	fs.set(filepath.Join(parent, "cgroup.events"), "populated 1\n")
	fs.onPause = func() { fs.set(filepath.Join(parent, "cgroup.events"), "populated 0\n") }
	fs.dirs[filepath.Join(parent, "unknown")] = true
	if result := execution.Close(); result.failureKind() != FailureContainmentCleanup {
		t.Fatalf("Close() with unknown entry = %#v, want cleanup failure", result)
	}
	if !fs.dirs[filepath.Join(parent, "unknown")] {
		t.Fatal("Close removed an unknown directory")
	}
}

func TestLinuxCloseRemovesOwnedHierarchyDeepestFirstAndRejectsDuplicate(t *testing.T) {
	t.Parallel()

	fs := newFakeCgroupFS("/delegated")
	execution := beginFakeExecution(t, fs, &fakeLauncher{processes: []*fakeProcess{{result: processResult{}}}}, media.DefaultLimits())
	if result := execution.Run(testCommand(), io.Discard, io.Discard); !result.succeeded() {
		t.Fatalf("Run() = %#v", result)
	}
	parent := fs.ownedParent()
	if result := execution.Close(); !result.succeeded() {
		t.Fatalf("Close() = %#v", result)
	}
	operations := fs.operations()
	childRemoval := indexContains(operations, "remove "+filepath.Join(parent, "command-1"))
	parentRemoval := indexContains(operations, "remove "+parent)
	if childRemoval < 0 || parentRemoval <= childRemoval || fs.ownedDirectoryCount() != 0 {
		t.Fatalf("operations = %#v, want child then parent removal", operations)
	}
	if result := execution.Close(); result.failureKind() != FailureInternal {
		t.Fatalf("duplicate Close() = %#v, want internal failure", result)
	}
}

func TestLinuxSysProcAttrUsesCloneIntoCgroupAndProcessGroup(t *testing.T) {
	t.Parallel()

	attr := linuxSysProcAttr(42)
	if !attr.UseCgroupFD || attr.CgroupFD != 42 || !attr.Setpgid || attr.Pgid != 0 || attr.Cloneflags != 0 {
		t.Fatalf("SysProcAttr = %#v", attr)
	}
}

func beginFakeExecution(t *testing.T, fs *fakeCgroupFS, launcher *fakeLauncher, limits media.Limits) Execution {
	t.Helper()
	return beginFakeExecutionContext(t, context.Background(), fs, launcher, limits)
}

func beginFakeExecutionContext(t *testing.T, ctx context.Context, fs *fakeCgroupFS, launcher *fakeLauncher, limits media.Limits) Execution {
	t.Helper()
	runner := newLinuxRunner("/delegated", linuxRunnerDeps{
		fs: fs, launcher: launcher, pollInterval: time.Millisecond, cleanupTimeout: 10 * time.Millisecond,
		newTimer: newImmediateExecutionTimer,
	})
	execution, result := runner.Begin(ctx, limits)
	if !result.succeeded() {
		t.Fatalf("Begin() = %#v", result)
	}
	return execution
}

func testCommand() Command {
	return Command{Path: "/tool", Args: []string{"arg"}, Env: []string{"A=B"}, Dir: "/work"}
}

type fakeCgroupFS struct {
	mu              sync.Mutex
	root            string
	files           map[string]string
	dirs            map[string]bool
	omitOnCreate    map[string]bool
	createdDefaults map[string]string
	log             []string
	nextFD          int
	onPause         func()
}

func newFakeCgroupFS(root string) *fakeCgroupFS {
	return &fakeCgroupFS{
		root: root, dirs: map[string]bool{root: true}, omitOnCreate: map[string]bool{}, nextFD: 10,
		files: map[string]string{
			filepath.Join(root, "cgroup.controllers"):     "cpu memory pids\n",
			filepath.Join(root, "cgroup.subtree_control"): "",
			filepath.Join(root, "cgroup.kill"):            "",
			filepath.Join(root, "cgroup.type"):            "domain\n",
		},
		createdDefaults: map[string]string{
			"cgroup.kill": "", "cgroup.events": "populated 0\n", "cgroup.type": "domain\n", "cgroup.controllers": "cpu memory pids\n",
			"cgroup.subtree_control": "", "pids.max": "max\n", "memory.max": "max\n", "memory.swap.max": "max\n",
			"cpu.max": "max 100000\n", "cpu.stat": "usage_usec 0\n", "memory.events": "max 0\noom 0\noom_kill 0\n", "pids.events": "max 0\n",
		},
	}
}

func (fs *fakeCgroupFS) ReadFile(path string) ([]byte, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	value, ok := fs.files[path]
	if !ok {
		return nil, errors.New("missing")
	}
	return []byte(value), nil
}
func (fs *fakeCgroupFS) WriteFile(path string, data []byte) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, ok := fs.files[path]; !ok {
		return errors.New("missing")
	}
	fs.files[path] = string(data)
	fs.log = append(fs.log, "write "+path+"="+string(data))
	return nil
}
func (fs *fakeCgroupFS) Exists(path string) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	_, ok := fs.files[path]
	return ok
}
func (fs *fakeCgroupFS) Mkdir(path string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.dirs[path] {
		return errors.New("exists")
	}
	fs.dirs[path] = true
	fs.log = append(fs.log, "mkdir "+path)
	for name, value := range fs.createdDefaults {
		if !fs.omitOnCreate[name] {
			fs.files[filepath.Join(path, name)] = value
		}
	}
	return nil
}
func (fs *fakeCgroupFS) OpenDir(path string) (cgroupDir, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !fs.dirs[path] {
		return nil, errors.New("missing")
	}
	fs.nextFD++
	return fakeDir{fd: fs.nextFD}, nil
}
func (fs *fakeCgroupFS) Remove(path string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for child := range fs.dirs {
		if child != path && filepath.Dir(child) == path {
			return errors.New("not empty")
		}
	}
	if !fs.dirs[path] {
		return errors.New("missing")
	}
	delete(fs.dirs, path)
	for file := range fs.files {
		if filepath.Dir(file) == path {
			delete(fs.files, file)
		}
	}
	fs.log = append(fs.log, "remove "+path)
	return nil
}
func (fs *fakeCgroupFS) Pause(context.Context, time.Duration) bool {
	if fs.onPause != nil {
		fs.onPause()
	}
	return true
}
func (fs *fakeCgroupFS) set(path, value string) { fs.mu.Lock(); fs.files[path] = value; fs.mu.Unlock() }
func (fs *fakeCgroupFS) writeLog() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []string
	for _, item := range fs.log {
		if strings.HasPrefix(item, "write ") {
			out = append(out, strings.TrimPrefix(item, "write "))
		}
	}
	return out
}
func (fs *fakeCgroupFS) operations() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]string(nil), fs.log...)
}
func (fs *fakeCgroupFS) firstMkdirIndex() int { return indexPrefix(fs.operations(), "mkdir ") }
func (fs *fakeCgroupFS) firstLimitWriteIndex() int {
	ops := fs.operations()
	for i, op := range ops {
		if strings.Contains(op, "/pids.max=") {
			return i
		}
	}
	return -1
}
func (fs *fakeCgroupFS) ownedDirectoryCount() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.dirs) - 1
}
func (fs *fakeCgroupFS) directories() map[string]bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := map[string]bool{}
	for k, v := range fs.dirs {
		out[k] = v
	}
	return out
}
func (fs *fakeCgroupFS) ownedParent() string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for dir := range fs.dirs {
		if filepath.Dir(dir) == fs.root {
			return dir
		}
	}
	return ""
}
func (fs *fakeCgroupFS) killBefore(wait <-chan struct{}) bool {
	select {
	case <-wait:
	default:
		return false
	}
	for _, op := range fs.operations() {
		if strings.Contains(op, "/cgroup.kill=1") {
			return true
		}
	}
	return false
}

type fakeDir struct{ fd int }

func (dir fakeDir) Fd() uintptr { return uintptr(dir.fd) }
func (fakeDir) Close() error    { return nil }

type immediateExecutionTimer struct{ channel chan time.Time }

func newImmediateExecutionTimer(time.Duration) executionTimer {
	channel := make(chan time.Time, 1)
	channel <- time.Time{}
	return &immediateExecutionTimer{channel: channel}
}
func (timer *immediateExecutionTimer) Channel() <-chan time.Time { return timer.channel }
func (*immediateExecutionTimer) Stop() bool                      { return true }

type fakeLauncher struct {
	mu         sync.Mutex
	processes  []*fakeProcess
	specs      []launchSpec
	afterStart func(parent string)
	failure    launchFailure
}

func (launcher *fakeLauncher) Start(spec launchSpec, _, _ io.Writer) (startedProcess, launchFailure) {
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	launcher.specs = append(launcher.specs, spec)
	if launcher.failure.unavailable || launcher.failure.internal {
		return nil, launcher.failure
	}
	if len(launcher.processes) == 0 {
		return nil, launchFailure{internal: true}
	}
	process := launcher.processes[0]
	launcher.processes = launcher.processes[1:]
	process.initialize()
	process.markStarted()
	if launcher.afterStart != nil {
		launcher.afterStart(filepath.Dir(spec.cgroupPath))
	}
	return process, launchFailure{}
}

type fakeProcess struct {
	result       processResult
	block        bool
	started      chan struct{}
	release      chan struct{}
	waitObserved chan struct{}
	once         sync.Once
	startedOnce  sync.Once
}

func (process *fakeProcess) initialize() {
	process.once.Do(func() {
		process.started = make(chan struct{})
		process.release = make(chan struct{})
		process.waitObserved = make(chan struct{})
	})
}
func (process *fakeProcess) markStarted() { process.startedOnce.Do(func() { close(process.started) }) }
func (process *fakeProcess) Wait() processResult {
	if process.block {
		<-process.release
	}
	close(process.waitObserved)
	return process.result
}
func (process *fakeProcess) KillGroup() {
	if process.block {
		select {
		case <-process.release:
		default:
			close(process.release)
		}
	}
}

func containsSuffix(values []string, suffix string) bool {
	for _, value := range values {
		if strings.HasSuffix(value, suffix) {
			return true
		}
	}
	return false
}
func indexPrefix(values []string, prefix string) int {
	for index, value := range values {
		if strings.HasPrefix(value, prefix) {
			return index
		}
	}
	return -1
}

func indexContains(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}

var _ = syscall.SysProcAttr{}
