package nativemode

import "strings"

func applyLinux(in *ApplyInput) {
	if in == nil || in.NodeDef == nil || !strings.EqualFold(in.NodeDef.Kind, "linux") {
		return
	}

	cmd := strings.TrimSpace(in.NodeDef.Cmd)
	if cmd == "" {
		lc := strings.ToLower(strings.TrimSpace(in.NodeImage))
		// Common netlab examples use python/alpine images for linux hosts. In Kubernetes,
		// these can exit immediately when STDIN is not attached. Force a long-running
		// command so the node stays up for exec/config steps.
		if strings.Contains(lc, "python") || strings.Contains(lc, "alpine") {
			cmd = "sleep infinity"
		}
	}

	if cmd != "" {
		in.NOS.Command = []string{"sh", "-c", cmd}
	}
}
