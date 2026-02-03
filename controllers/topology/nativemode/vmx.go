package nativemode

import (
	"slices"
	"strings"

	k8scorev1 "k8s.io/api/core/v1"
)

func applyJuniperVMX(in *ApplyInput) {
	if in == nil || in.NOS == nil {
		return
	}

	// vMX is a qemu-based vrnetlab image. In native mode, clabernetes expects the management
	// plane to remain on the Kubernetes pod network. vrnetlab's management passthrough mode
	// requires creating a tap interface and running tc in the NOS container, which is not
	// guaranteed in clabernetes' native-mode security model. Keep management in the default
	// host-forwarded mode.
	upsertEnv := func(key, value string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}

		for i := range in.NOS.Env {
			if strings.TrimSpace(in.NOS.Env[i].Name) == key {
				in.NOS.Env[i].Value = value

				return
			}
		}

		in.NOS.Env = append(in.NOS.Env, k8scorev1.EnvVar{Name: key, Value: value})
	}

	upsertEnv("CLAB_MGMT_PASSTHROUGH", "false")

	// netlab expects vMX to be reachable with admin/admin@123 by default.
	// vrnetlab supports setting these via command-line flags.
	if !slices.Contains(in.NOS.Args, "--hostname") && strings.TrimSpace(in.NodeName) != "" {
		in.NOS.Args = append(in.NOS.Args, "--hostname", strings.TrimSpace(in.NodeName))
	}
	if !slices.Contains(in.NOS.Args, "--username") {
		in.NOS.Args = append(in.NOS.Args, "--username", "admin")
	}
	if !slices.Contains(in.NOS.Args, "--password") {
		in.NOS.Args = append(in.NOS.Args, "--password", "admin@123")
	}

	// The vr-vmx vrnetlab image supports multiple datapath connection modes.
	// In clabernetes native mode we rely on the same "tc + tap" datapath wiring
	// that containerlab uses (bridge/tap + tc mirroring).
	//
	// Without this, some vrnetlab versions emit QEMU args that reference a
	// netdev id without defining it (e.g. p01), causing QEMU startup failures.
	if !slices.Contains(in.NOS.Args, "--connection-mode") && !slices.Contains(in.NOS.Args, "--connectionMode") {
		in.NOS.Args = append(in.NOS.Args, "--connection-mode", "tc")
	}
}
