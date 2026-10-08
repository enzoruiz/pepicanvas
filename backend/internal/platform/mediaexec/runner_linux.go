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
	return newCgroupRunner(root, linuxRunnerDeps{})
}

func newCgroupRunner(root string, deps linuxRunnerDeps) (Runner, Result) {
	deps = completeLinuxRunnerDeps(deps)
	canonical, ok := canonicalDelegatedRoot(root, deps.readMountInfo)
	if !ok {
		return nil, FailedResult(FailureContainmentSetup)
	}
	return &linuxRunner{root: canonical, deps: deps}, SuccessfulResult()
}

type delegatedRoot struct {
	path     string
	identity fileIdentity
}

type fileIdentity struct {
	device uint64
	inode  uint64
}

func canonicalDelegatedRoot(root string, readMountInfo func() ([]byte, error)) (delegatedRoot, bool) {
	if strings.IndexByte(root, 0) >= 0 || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return delegatedRoot{}, false
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || !filepath.IsAbs(canonical) || canonical == string(filepath.Separator) || canonical == "/sys/fs/cgroup" {
		return delegatedRoot{}, false
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return delegatedRoot{}, false
	}
	if _, err := os.Stat(filepath.Join(canonical, "cgroup.kill")); err != nil {
		return delegatedRoot{}, false
	}
	identity, ok := identityFromFileInfo(info)
	if !ok || readMountInfo == nil {
		return delegatedRoot{}, false
	}
	mountInfo, err := readMountInfo()
	mount, mountOK := cgroup2MountForRoot(canonical, mountInfo)
	if err != nil || !mountOK || !mountDeviceMatches(identity.device, mount) {
		return delegatedRoot{}, false
	}
	return delegatedRoot{path: canonical, identity: identity}, true
}

type cgroupMount struct {
	point        string
	filesystem   string
	mountOptions map[string]bool
	superOptions map[string]bool
	major        uint64
	minor        uint64
}

func validCgroup2Mount(root string, data []byte) bool {
	_, ok := cgroup2MountForRoot(root, data)
	return ok
}

func cgroup2MountForRoot(root string, data []byte) (cgroupMount, bool) {
	mounts, ok := parseMountInfo(data)
	if !ok {
		return cgroupMount{}, false
	}
	var selected *cgroupMount
	for index := range mounts {
		mount := &mounts[index]
		if !pathContains(mount.point, root) {
			continue
		}
		if selected == nil || len(mount.point) > len(selected.point) {
			selected = mount
			continue
		}
		if len(mount.point) == len(selected.point) {
			return cgroupMount{}, false
		}
	}
	if selected == nil || selected.filesystem != "cgroup2" {
		return cgroupMount{}, false
	}
	for _, option := range []string{"memory_localevents", "pids_localevents"} {
		if selected.mountOptions[option] || selected.superOptions[option] {
			return cgroupMount{}, false
		}
	}
	return *selected, true
}

func parseMountInfo(data []byte) ([]cgroupMount, bool) {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil, false
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	mounts := make([]cgroupMount, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			return nil, false
		}
		mountID, mountIDErr := strconv.ParseUint(fields[0], 10, 64)
		parentID, parentIDErr := strconv.ParseUint(fields[1], 10, 64)
		device := strings.Split(fields[2], ":")
		if mountIDErr != nil || parentIDErr != nil || mountID == 0 || parentID == 0 || len(device) != 2 {
			return nil, false
		}
		major, majorErr := strconv.ParseUint(device[0], 10, 64)
		minor, minorErr := strconv.ParseUint(device[1], 10, 64)
		_, rootOK := decodeMountInfoPath(fields[3])
		if majorErr != nil || minorErr != nil || !rootOK {
			return nil, false
		}
		separator := -1
		for index := 6; index < len(fields); index++ {
			if fields[index] == "-" {
				separator = index
				break
			}
		}
		if separator < 6 || separator+3 >= len(fields) {
			return nil, false
		}
		point, ok := decodeMountInfoPath(fields[4])
		if !ok || !filepath.IsAbs(point) || filepath.Clean(point) != point {
			return nil, false
		}
		mounts = append(mounts, cgroupMount{
			point:        point,
			filesystem:   fields[separator+1],
			mountOptions: optionSet(fields[5]),
			superOptions: optionSet(fields[separator+3]),
			major:        major,
			minor:        minor,
		})
	}
	return mounts, true
}

func mountDeviceMatches(device uint64, mount cgroupMount) bool {
	major := ((device >> 8) & 0xfff) | ((device >> 32) & 0xfffff000)
	minor := (device & 0xff) | ((device >> 12) & 0xffffff00)
	return major == mount.major && minor == mount.minor
}

func decodeMountInfoPath(value string) (string, bool) {
	var decoded strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			decoded.WriteByte(value[index])
			continue
		}
		if index+3 >= len(value) {
			return "", false
		}
		escape := value[index+1 : index+4]
		switch escape {
		case "040":
			decoded.WriteByte(' ')
		case "011":
			decoded.WriteByte('\t')
		case "012":
			decoded.WriteByte('\n')
		case "134":
			decoded.WriteByte('\\')
		default:
			return "", false
		}
		index += 3
	}
	return decoded.String(), true
}

func optionSet(value string) map[string]bool {
	options := make(map[string]bool)
	for _, option := range strings.Split(value, ",") {
		if option != "" {
			options[option] = true
		}
	}
	return options
}

func pathContains(parent, child string) bool {
	return parent == string(filepath.Separator) || child == parent || strings.HasPrefix(child, parent+string(filepath.Separator))
}

func identityFromFileInfo(info os.FileInfo) (fileIdentity, bool) {
	if info == nil {
		return fileIdentity{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Dev == 0 || stat.Ino == 0 {
		return fileIdentity{}, false
	}
	return fileIdentity{device: uint64(stat.Dev), inode: stat.Ino}, true
}

type osCgroupRoot struct {
	file *os.File
	path string
}

func openDelegatedRoot(path string, expected fileIdentity) (cgroupRoot, error) {
	if expected == (fileIdentity{}) {
		return nil, errors.New("missing delegated root identity")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (cgroupRoot, error) {
		_ = file.Close()
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.IsDir() {
		return fail(errors.New("invalid delegated root descriptor"))
	}
	actual, ok := identityFromFileInfo(info)
	if !ok || actual != expected {
		return fail(errors.New("delegated root identity changed"))
	}
	capability := "/proc/self/fd/" + strconv.FormatUint(uint64(file.Fd()), 10)
	target, err := os.Readlink(capability)
	if err != nil || filepath.Clean(target) != path {
		return fail(errors.New("delegated root descriptor resolution changed"))
	}
	resolved, err := filepath.EvalSymlinks(capability)
	if err != nil || resolved != path {
		return fail(errors.New("delegated root capability cannot be proven"))
	}
	capabilityInfo, err := os.Stat(capability)
	capabilityIdentity, identityOK := identityFromFileInfo(capabilityInfo)
	if err != nil || !identityOK || capabilityIdentity != expected {
		return fail(errors.New("delegated root capability identity changed"))
	}
	return &osCgroupRoot{file: file, path: capability}, nil
}

func (root *osCgroupRoot) Fd() uintptr  { return root.file.Fd() }
func (root *osCgroupRoot) Path() string { return root.path }
func (root *osCgroupRoot) Close() error { return root.file.Close() }

type linuxRunner struct {
	root delegatedRoot
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
	readMountInfo  func() ([]byte, error)
}

func newLinuxRunner(root string, deps linuxRunnerDeps) *linuxRunner {
	deps = completeLinuxRunnerDeps(deps)
	return &linuxRunner{root: delegatedRoot{path: root}, deps: deps}
}

func completeLinuxRunnerDeps(deps linuxRunnerDeps) linuxRunnerDeps {
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
	if deps.readMountInfo == nil {
		deps.readMountInfo = func() ([]byte, error) { return os.ReadFile("/proc/self/mountinfo") }
	}
	return deps
}

func (runner *linuxRunner) Begin(ctx context.Context, limits media.Limits) (Execution, Result) {
	if ctx == nil || limits.Validate() != nil {
		return nil, FailedResult(FailureContainmentSetup)
	}
	if result := contextFailure(ctx); result.valid() {
		return nil, result
	}
	root, err := runner.deps.fs.OpenRoot(runner.root.path, runner.root.identity)
	if err != nil {
		return nil, FailedResult(FailureContainmentUnavailable)
	}
	rootPath := root.Path()
	closeRoot := func(result Result) (Execution, Result) {
		if root.Close() != nil {
			return nil, FailedResult(FailureContainmentCleanup)
		}
		return nil, result
	}
	if runner.root.identity != (fileIdentity{}) {
		mountInfo, readErr := runner.deps.readMountInfo()
		mount, mountOK := cgroup2MountForRoot(runner.root.path, mountInfo)
		if readErr != nil || !mountOK || !mountDeviceMatches(runner.root.identity.device, mount) {
			return closeRoot(FailedResult(FailureContainmentUnavailable))
		}
	}
	if !runner.deps.fs.Exists(filepath.Join(rootPath, "cgroup.kill")) {
		return closeRoot(FailedResult(FailureContainmentUnavailable))
	}
	cgroupType, err := runner.deps.fs.ReadFile(filepath.Join(rootPath, "cgroup.type"))
	if err != nil || strings.TrimSpace(string(cgroupType)) != "domain" {
		return closeRoot(FailedResult(FailureContainmentUnavailable))
	}
	controllers, err := runner.deps.fs.ReadFile(filepath.Join(rootPath, "cgroup.controllers"))
	if err != nil || !containsControllers(controllers, requiredControllers) {
		return closeRoot(FailedResult(FailureContainmentUnavailable))
	}
	subtree, err := runner.deps.fs.ReadFile(filepath.Join(rootPath, "cgroup.subtree_control"))
	if err != nil || !containsControllers(subtree, requiredControllers) {
		return closeRoot(FailedResult(FailureContainmentUnavailable))
	}
	name, err := runner.deps.name()
	if err != nil || name == "" || filepath.Base(name) != name {
		return closeRoot(FailedResult(FailureContainmentSetup))
	}
	parent := filepath.Join(rootPath, name)
	if err := runner.deps.fs.Mkdir(parent); err != nil {
		return closeRoot(FailedResult(FailureContainmentSetup))
	}
	execution := &linuxExecution{ctx: ctx, limits: limits, root: root, parent: parent, deps: runner.deps}
	if result := execution.configure(); !result.succeeded() {
		removeErr := runner.deps.fs.Remove(parent)
		closeErr := root.Close()
		if removeErr != nil || closeErr != nil {
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
	root     cgroupRoot
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
	if result := contextFailure(execution.ctx); result.valid() {
		return result
	}
	usage, err := execution.cpuUsage()
	if err != nil || usage < execution.baseline {
		return FailedResult(FailureInternal)
	}
	if usage-execution.baseline >= uint64(execution.limits.MaxCPUTime/time.Microsecond) {
		return FailedResult(FailureCPULimit)
	}
	if forced.valid() {
		return forced
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
	if execution.root == nil || execution.root.Close() != nil {
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
	OpenRoot(string, fileIdentity) (cgroupRoot, error)
	Remove(string) error
	Pause(context.Context, time.Duration) bool
}

type cgroupDir interface {
	Fd() uintptr
	Close() error
}

type cgroupRoot interface {
	cgroupDir
	Path() string
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
func (osCgroupFS) OpenRoot(path string, identity fileIdentity) (cgroupRoot, error) {
	root, err := openDelegatedRoot(path, identity)
	if err != nil {
		return nil, err
	}
	osRoot, ok := root.(*osCgroupRoot)
	if !ok {
		_ = root.Close()
		return nil, errors.New("invalid delegated root capability")
	}
	var filesystem syscall.Statfs_t
	if err := syscall.Fstatfs(int(osRoot.file.Fd()), &filesystem); err != nil || uint64(filesystem.Type) != 0x63677270 {
		_ = root.Close()
		return nil, errors.New("delegated root is not cgroup2")
	}
	return root, nil
}
func (osCgroupFS) Remove(path string) error { return os.Remove(path) }
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
