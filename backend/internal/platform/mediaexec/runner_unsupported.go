//go:build !linux

package mediaexec

// NewCgroupRunner exposes the platform-neutral constructor contract while
// failing closed on systems without Linux cgroup-v2 clone-into-cgroup support.
func NewCgroupRunner(string) (Runner, Result) {
	return nil, FailedResult(FailureContainmentUnavailable)
}
