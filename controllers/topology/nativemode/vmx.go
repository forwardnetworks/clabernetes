package nativemode

import (
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
}
