//go:build linux

package mediaexec

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

func TestLinuxMountValidationRequiresRecursiveCgroup2Events(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		root string
		data string
		want bool
	}{
		{name: "normal", root: "/sys/fs/cgroup/team", data: "29 23 0:26 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw\n", want: true},
		{name: "memory localevents mount option", root: "/sys/fs/cgroup/team", data: "29 23 0:26 / /sys/fs/cgroup rw,memory_localevents - cgroup2 cgroup rw\n"},
		{name: "pids localevents super option", root: "/sys/fs/cgroup/team", data: "29 23 0:26 / /sys/fs/cgroup rw - cgroup2 cgroup rw,pids_localevents\n"},
		{name: "both localevents options", root: "/sys/fs/cgroup/team", data: "29 23 0:26 / /sys/fs/cgroup rw,memory_localevents - cgroup2 cgroup rw,pids_localevents\n"},
		{name: "malformed input", root: "/sys/fs/cgroup/team", data: "29 23 0:26 / /sys/fs/cgroup rw cgroup2\n"},
		{name: "malformed escape", root: "/sys/fs/cgroup/team", data: "29 23 0:26 / /sys/fs/cgroup\\099 rw - cgroup2 cgroup rw\n"},
		{name: "escaped mount point", root: "/sys/fs/cgroup/team space/job", data: "29 23 0:26 / /sys/fs/cgroup/team\\040space rw - cgroup2 cgroup rw\n", want: true},
		{name: "non cgroup2", root: "/sys/fs/cgroup/team", data: "29 23 0:26 / /sys/fs/cgroup rw - tmpfs tmpfs rw\n"},
		{
			name: "longest containing mount wins",
			root: "/sys/fs/cgroup/team/job",
			data: "29 23 0:26 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n" +
				"30 29 0:27 / /sys/fs/cgroup/team rw - tmpfs tmpfs rw\n",
		},
		{
			name: "longest cgroup2 mount wins",
			root: "/sys/fs/cgroup/team/job",
			data: "29 23 0:26 / /sys/fs/cgroup rw - tmpfs tmpfs rw\n" +
				"30 29 0:27 / /sys/fs/cgroup/team rw - cgroup2 cgroup rw\n",
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validCgroup2Mount(tt.root, []byte(tt.data)); got != tt.want {
				t.Fatalf("validCgroup2Mount(%q) = %t, want %t", tt.root, got, tt.want)
			}
		})
	}
}

func TestNewCgroupRunnerUsesInjectedMountInfo(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.kill"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	data := "29 23 0:26 / " + root + " rw - cgroup2 cgroup rw,memory_localevents\n"
	runner, result := newCgroupRunner(root, linuxRunnerDeps{readMountInfo: func() ([]byte, error) {
		called = true
		return []byte(data), nil
	}})
	if !called || runner != nil || result.failureKind() != FailureContainmentSetup {
		t.Fatalf("newCgroupRunner() = %#v, %#v, called=%t; want fail-closed injected mount rejection", runner, result, called)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := identityFromFileInfo(info)
	if !ok {
		t.Fatal("temporary delegated root has no stable identity")
	}
	major := ((identity.device >> 8) & 0xfff) | ((identity.device >> 32) & 0xfffff000)
	minor := (identity.device & 0xff) | ((identity.device >> 12) & 0xffffff00)
	device := strconv.FormatUint(major, 10) + ":" + strconv.FormatUint(minor, 10)
	validData := "29 23 " + device + " / " + root + " rw - cgroup2 cgroup rw\n"
	runner, result = newCgroupRunner(root, linuxRunnerDeps{readMountInfo: func() ([]byte, error) { return []byte(validData), nil }})
	if runner == nil || !result.succeeded() {
		t.Fatalf("newCgroupRunner() = %#v, %#v; want proven mount identity", runner, result)
	}
	mismatchData := "29 23 0:0 / " + root + " rw - cgroup2 cgroup rw\n"
	runner, result = newCgroupRunner(root, linuxRunnerDeps{readMountInfo: func() ([]byte, error) { return []byte(mismatchData), nil }})
	if runner != nil || result.failureKind() != FailureContainmentSetup {
		t.Fatalf("newCgroupRunner() = %#v, %#v; want mount identity mismatch rejection", runner, result)
	}
}

func TestOpenDelegatedRootCapabilitySurvivesPathReplacement(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	path := filepath.Join(base, "delegated")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := identityFromFileInfo(info)
	if !ok {
		t.Fatal("temporary delegated root has no stable identity")
	}
	root, err := openDelegatedRoot(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(base, "original")
	if err := os.Rename(path, oldPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(root.Path(), "owned")
	if err := os.Mkdir(owned, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(owned, "marker")
	if err := os.WriteFile(marker, []byte("bound"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "bound" {
		t.Fatalf("capability read = %q, %v", data, err)
	}
	directory, err := os.Open(owned)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(oldPath, "owned", "marker")); err != nil {
		t.Fatalf("capability operation did not target original directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "owned")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement directory was targeted: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(owned); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
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
	for _, write := range writes {
		path := strings.SplitN(write, "=", 2)[0]
		if filepath.Dir(path) == "/delegated" {
			t.Fatalf("delegated root was mutated: %q", write)
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
	if closeResult := execution.Close(); !closeResult.succeeded() {
		t.Fatalf("Close() = %#v", closeResult)
	}
	operations := fs.operations()
	parent := fs.lastOwnedParent
	setupKill := indexNth(operations, "write "+filepath.Join(parent, "cgroup.kill")+"=1", 1)
	start := indexContains(operations, "process start")
	cancelKill := indexNth(operations, "write "+filepath.Join(parent, "cgroup.kill")+"=1", 2)
	groupKill := indexContains(operations, "process-group fallback")
	reap := indexContains(operations, "wait/reap")
	closeKill := indexNth(operations, "write "+filepath.Join(parent, "cgroup.kill")+"=1", 3)
	populated := indexContains(operations, "read "+filepath.Join(parent, "cgroup.events"))
	childRemoval := indexContains(operations, "remove "+filepath.Join(parent, "command-1"))
	parentRemoval := indexContains(operations, "remove "+parent)
	rootClose := indexContains(operations, "close root")
	if countExact(operations, "close root") != 1 || !(setupKill < start && start < cancelKill && cancelKill < groupKill && groupKill < reap && reap < closeKill && closeKill < populated && populated < childRemoval && childRemoval < parentRemoval && parentRemoval < rootClose) {
		t.Fatalf("operations = %#v, want setup kill < start < cancellation kill < process-group fallback < reap < close kill < populated < child removal < parent removal < root close", operations)
	}
}

func TestLinuxRunPostReapPrecedenceIsDeterministic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		after  map[string]string
		cancel bool
		want   FailureKind
	}{
		{name: "memory before context", after: map[string]string{"memory.events": "max 1\noom 0\noom_kill 0\n"}, cancel: true, want: FailureMemoryLimit},
		{name: "tasks before context", after: map[string]string{"pids.events": "max 1\n"}, cancel: true, want: FailureTaskLimit},
		{name: "context before CPU and exit", after: map[string]string{"cpu.stat": "usage_usec 30000000\n"}, cancel: true, want: FailureCancelled},
		{name: "CPU before process exit", after: map[string]string{"cpu.stat": "usage_usec 30000000\n"}, want: FailureCPULimit},
		{name: "context before process exit", cancel: true, want: FailureCancelled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeCgroupFS("/delegated")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			launcher := &fakeLauncher{processes: []*fakeProcess{{result: processResult{exited: true}}}}
			launcher.afterStart = func(parent string) {
				for name, value := range tt.after {
					fs.set(filepath.Join(parent, name), value)
				}
				if tt.cancel {
					cancel()
				}
			}
			execution := beginFakeExecutionContext(t, ctx, fs, launcher, media.DefaultLimits())
			if result := execution.Run(testCommand(), io.Discard, io.Discard); result.failureKind() != tt.want {
				t.Fatalf("Run() = %#v, want %v", result, tt.want)
			}
		})
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
	launcher.record = fs.record
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
	lastOwnedParent string
}

func newFakeCgroupFS(root string) *fakeCgroupFS {
	return &fakeCgroupFS{
		root: root, dirs: map[string]bool{root: true}, omitOnCreate: map[string]bool{}, nextFD: 10,
		files: map[string]string{
			filepath.Join(root, "cgroup.controllers"):     "cpu memory pids\n",
			filepath.Join(root, "cgroup.subtree_control"): "cpu memory pids\n",
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
	fs.log = append(fs.log, "read "+path)
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
	if filepath.Dir(path) == fs.root {
		fs.lastOwnedParent = path
	}
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
	return &fakeDir{fd: fs.nextFD}, nil
}
func (fs *fakeCgroupFS) OpenRoot(path string, _ fileIdentity) (cgroupRoot, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if path != fs.root || !fs.dirs[path] {
		return nil, errors.New("missing")
	}
	fs.nextFD++
	fs.log = append(fs.log, "open root")
	return &fakeRoot{fakeDir: fakeDir{fd: fs.nextFD}, fs: fs, path: path}, nil
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
func (fs *fakeCgroupFS) record(operation string) {
	fs.mu.Lock()
	fs.log = append(fs.log, operation)
	fs.mu.Unlock()
}
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

func (dir *fakeDir) Fd() uintptr { return uintptr(dir.fd) }
func (*fakeDir) Close() error    { return nil }

type fakeRoot struct {
	fakeDir
	fs   *fakeCgroupFS
	path string
}

func (root *fakeRoot) Path() string { return root.path }
func (root *fakeRoot) Close() error {
	root.fs.record("close root")
	return nil
}

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
	record     func(string)
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
	process.record = launcher.record
	if launcher.record != nil {
		launcher.record("process start")
	}
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
	record       func(string)
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
	if process.record != nil {
		process.record("wait/reap")
	}
	close(process.waitObserved)
	return process.result
}
func (process *fakeProcess) KillGroup() {
	if process.record != nil {
		process.record("process-group fallback")
	}
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

func indexNth(values []string, target string, occurrence int) int {
	seen := 0
	for index, value := range values {
		if value == target {
			seen++
			if seen == occurrence {
				return index
			}
		}
	}
	return -1
}

func countExact(values []string, target string) int {
	count := 0
	for _, value := range values {
		if value == target {
			count++
		}
	}
	return count
}

var _ = syscall.SysProcAttr{}
