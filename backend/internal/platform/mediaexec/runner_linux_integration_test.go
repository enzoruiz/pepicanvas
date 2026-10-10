//go:build linux

package mediaexec

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
)

func TestCgroupRunnerIntegration(t *testing.T) {
	root := os.Getenv("PEPICANVAS_TEST_CGROUP_ROOT")
	if root == "" {
		t.Skip("PEPICANVAS_TEST_CGROUP_ROOT is not set to a disposable delegated cgroup")
	}
	runner, result := NewCgroupRunner(root)
	if !result.succeeded() {
		t.Fatalf("NewCgroupRunner() = %#v", result)
	}
	execution, result := runner.Begin(context.Background(), media.DefaultLimits())
	if !result.succeeded() {
		t.Fatalf("Begin() = %#v", result)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := Command{
		Path: testBinary,
		Args: []string{"-test.run=^TestCgroupRunnerIntegrationDescendantHelper$"},
		Env:  []string{"PEPICANVAS_CGROUP_HELPER=1"},
		Dir:  filepath.Clean("/tmp"),
	}
	if result := execution.Run(command, io.Discard, io.Discard); !result.succeeded() {
		t.Fatalf("Run() = %#v", result)
	}
	if result := execution.Close(); !result.succeeded() {
		t.Fatalf("Close() = %#v", result)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "pepicanvas-media-") {
			t.Fatalf("owned cgroup remains after Close: %q", entry.Name())
		}
	}
}

func TestCgroupRunnerIntegrationDescendantHelper(t *testing.T) {
	if os.Getenv("PEPICANVAS_CGROUP_HELPER") != "1" {
		t.Skip("integration helper")
	}
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
