//go:build !linux

package mediaexec

import "testing"

func TestCgroupRunnerFailsClosedOnUnsupportedPlatform(t *testing.T) {
	runner, result := NewCgroupRunner("/delegated")
	if runner != nil || result.failureKind() != FailureContainmentUnavailable {
		t.Fatalf("NewCgroupRunner() = %#v, %#v", runner, result)
	}
}
