package launcher

import (
	"os"
	"path/filepath"
	"strings"

	clabernetesconstants "github.com/srl-labs/clabernetes/constants"
)

const defaultRuntimeDir = "/clabernetes/runtime"

func runtimeDir() string {
	dir := strings.TrimSpace(os.Getenv(clabernetesconstants.LauncherRuntimeDirEnv))
	if dir == "" {
		return defaultRuntimeDir
	}

	return dir
}

func runtimePath(name string) string {
	return filepath.Join(runtimeDir(), name)
}
