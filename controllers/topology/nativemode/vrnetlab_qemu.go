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
