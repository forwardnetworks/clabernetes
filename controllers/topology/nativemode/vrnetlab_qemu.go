package nativemode

import (
	"strings"

	k8scorev1 "k8s.io/api/core/v1"
)

func applyVrnetlabQemuNative(in *ApplyInput) {
	if in == nil || in.NOS == nil || in.NodeDef == nil {
		return
	}

	kind := strings.ToLower(strings.TrimSpace(in.NodeDef.Kind))
	if kind == "" {
		return
	}

	img := strings.ToLower(strings.TrimSpace(in.NodeImage))
	if img == "" || !strings.Contains(img, "/vrnetlab/") {
		return
	}

	if kind == "cisco_iol" || kind == "cisco_ioll2" {
		return
	}

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

	// In native mode we keep management connectivity on the Kubernetes pod network.
	// Management passthrough mode requires creating tap interfaces and running tc in the
	// NOS container, which is not guaranteed in clabernetes' native-mode security model.
	upsertEnv("CLAB_MGMT_PASSTHROUGH", "false")

	// vrnetlab qemu-based nodes generally expect a privileged container (as in the standard
	// `docker run --privileged ...` workflow) to create TAP interfaces and attach them to
	// the pod's veth endpoints. Keep this scoped to known qemu-based kinds.
	privileged := true
	runAsUser := int64(0)
	allowPrivilegeEscalation := true
	in.NOS.SecurityContext = &k8scorev1.SecurityContext{
		Privileged:               &privileged,
		RunAsUser:                &runAsUser,
		AllowPrivilegeEscalation: &allowPrivilegeEscalation,
	}
}
