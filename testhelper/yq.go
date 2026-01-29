package testhelper

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// YQCommand accepts some yaml content and returns it after executing the given yqPattern against
// it.
func YQCommand(t *testing.T, content []byte, yqPattern string) []byte {
	t.Helper()

	// Always use a local, repo-pinned yq to avoid requiring system installs.
	// NOTE: `go test` runs with varying working directories (per package), so
	// we use an absolute path based on the location of this source file.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("failed to locate testhelper/yq.go")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), ".."))
	yqPath := filepath.Join(repoRoot, "bin", "yq")
	yqCmd := fmt.Sprintf("echo '%s' | %s '%s'", string(content), yqPath, yqPattern)

	cmd := exec.CommandContext( //nolint:gosec
		t.Context(),
		"bash",
		"-c",
		yqCmd,
	)

	return Execute(t, cmd)
}
