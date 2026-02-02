package nativemode

import (
	"slices"
	"strings"

	clabernetesapisv1alpha1 "github.com/srl-labs/clabernetes/apis/v1alpha1"
	claberneteslogging "github.com/srl-labs/clabernetes/logging"
	clabernetesutilcontainerlab "github.com/srl-labs/clabernetes/util/containerlab"
	k8sappsv1 "k8s.io/api/apps/v1"
	k8scorev1 "k8s.io/api/core/v1"
)

// ApplyInput is the set of inputs used to apply native-mode overrides to a node deployment.
//
// Native mode runs the NOS container directly in the Kubernetes pod (instead of inside a
// containerlab-managed docker-in-docker runtime). Some node kinds require additional pod-spec
// configuration to boot and/or remain reachable.
type ApplyInput struct {
	OwningTopology *clabernetesapisv1alpha1.Topology
	NodeName       string
	NodeImage      string
	NodeDef        *clabernetesutilcontainerlab.NodeDefinition
	Links          []*clabernetesutilcontainerlab.LinkDefinition

	FilesFromConfigMap []clabernetesapisv1alpha1.FileFromConfigMap

	Deployment *k8sappsv1.Deployment
	NOS        *k8scorev1.Container
}

// ApplyNativeModeOverrides applies best-effort native-mode compatibility changes for a node.
//
// This function must never perform link "plumbing" inside the NOS. It may, however:
// - ensure required env vars are set for a NOS to boot,
// - mount startup-config or runtime artifacts expected by a NOS entrypoint,
// - override container command/entrypoint in cases where native-mode requires it.
func ApplyNativeModeOverrides(log claberneteslogging.Instance, in *ApplyInput) {
	if in == nil || in.Deployment == nil || in.NOS == nil {
		log.Warn("nativemode: missing deployment or NOS container, skipping native-mode overrides")

		return
	}

	if in.NodeDef == nil {
		return
	}

	kind := strings.TrimSpace(in.NodeDef.Kind)
	if kind == "" {
		return
	}

	applyEnvMap(in)
	applyLinux(in)
	applyBindMounts(in)

	switch strings.ToLower(kind) {
	case "ceos", "eos":
		applyCEOS(log, in)
	case "cisco_iol":
		applyCiscoIOL(log, in)
	case "cisco_vios", "cisco_viosl2":
		applyCiscoVIOS(log, in)
	case "vr-vmx":
		applyJuniperVMX(in)
	default:
		// other kinds are handled elsewhere or require no special native-mode changes
	}

	applyVrnetlabQemuNative(in)

	applyCommandOverride(in)

	// Ensure deterministic ordering for stable reconciles.
	slices.SortFunc(in.NOS.Env, func(a, b k8scorev1.EnvVar) int {
		return strings.Compare(a.Name, b.Name)
	})
}
