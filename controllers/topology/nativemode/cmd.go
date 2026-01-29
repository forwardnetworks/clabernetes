package nativemode

import "strings"

func applyCommandOverride(in *ApplyInput) {
	// Do not override commands already set by native-mode kind handlers (e.g., cEOS/IOL).
	if in == nil || len(in.NOS.Command) > 0 {
		return
	}

	if in.NodeDef == nil {
		return
	}

	cmd := strings.TrimSpace(in.NodeDef.Cmd)
	if cmd != "" {
		in.NOS.Command = []string{"sh", "-c", cmd}

		return
	}

	entrypoint := strings.TrimSpace(in.NodeDef.Entrypoint)
	if entrypoint != "" {
		in.NOS.Command = []string{"sh", "-c", entrypoint}
	}
}
