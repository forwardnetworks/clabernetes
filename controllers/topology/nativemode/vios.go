package nativemode

import (
	"strings"

	claberneteslogging "github.com/srl-labs/clabernetes/logging"
	clabernetesutilkubernetes "github.com/srl-labs/clabernetes/util/kubernetes"
	k8scorev1 "k8s.io/api/core/v1"
)

// applyCiscoVIOS ensures vrnetlab's IOSv/IOSvL2 container has the startup-config mounted at the
// location expected by upstream vrnetlab (`/config/startup-config.cfg`).
//
// This keeps our behavior aligned with stock vrnetlab/containerlab: vrnetlab itself does not
// "enable SSH by default"; it applies the injected startup-config which is responsible for
// enabling management services.
func applyCiscoVIOS(log claberneteslogging.Instance, in *ApplyInput) {
	if in == nil || in.NodeDef == nil {
		return
	}

	existingMounts, _ := collectExistingMountsAndVolumes(in)

	// Containerlab uses `startup-config` to point at a file; in clabernetes that file is materialized
	// via FilesFromConfigMap (per-node). We mount it to vrnetlab's expected path.
	startupConfigPath := strings.TrimSpace(in.NodeDef.StartupConfig)
	if startupConfigPath == "" {
		return
	}

	const vrnetlabStartupConfigMount = "/config/startup-config.cfg"
	if _, ok := existingMounts[vrnetlabStartupConfigMount]; ok {
		return
	}

	for _, f := range in.FilesFromConfigMap {
		if strings.TrimSpace(f.ConfigMapName) == "" || strings.TrimSpace(f.ConfigMapPath) == "" {
			continue
		}

		if strings.TrimSpace(f.FilePath) != startupConfigPath {
			continue
		}

		volumeName := clabernetesutilkubernetes.EnforceDNSLabelConvention(
			clabernetesutilkubernetes.SafeConcatNameKubernetes(
				f.ConfigMapName,
				f.ConfigMapPath,
			),
		)

		in.NOS.VolumeMounts = append(
			in.NOS.VolumeMounts,
			k8scorev1.VolumeMount{
				Name:      volumeName,
				ReadOnly:  true,
				MountPath: vrnetlabStartupConfigMount,
				SubPath:   f.ConfigMapPath,
			},
		)
		existingMounts[vrnetlabStartupConfigMount] = struct{}{}

		return
	}

	log.Warnf(
		"nativemode: vios startup-config not mounted for %q (%q)",
		in.NodeName,
		startupConfigPath,
	)
}
