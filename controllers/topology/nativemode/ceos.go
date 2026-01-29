package nativemode

import (
	"slices"
	"strings"

	claberneteslogging "github.com/srl-labs/clabernetes/logging"
	clabernetesutilkubernetes "github.com/srl-labs/clabernetes/util/kubernetes"
	k8scorev1 "k8s.io/api/core/v1"
)

func applyCEOS(log claberneteslogging.Instance, in *ApplyInput) {
	if in == nil {
		return
	}

	existingMounts, existingVolumes := collectExistingMountsAndVolumes(in)
	ensureSystemdTmpfsMounts(in, existingMounts, existingVolumes)

	startupConfigPath := strings.TrimSpace(in.NodeDef.StartupConfig)
	startupConfigMounted := mountCEOSStartupConfig(in, existingMounts, startupConfigPath)

	existingEnv := buildExistingEnv(in)
	applyCEOSEnv(in, existingEnv)
	setCEOSInitCommand(in, existingEnv)

	if !startupConfigMounted && startupConfigPath != "" {
		log.Warnf(
			"nativemode: ceos startup-config not mounted for %q (%q)",
			in.NodeName,
			startupConfigPath,
		)
	}
}

func collectExistingMountsAndVolumes(
	in *ApplyInput,
) (existingMounts, existingVolumes map[string]struct{}) {
	existingMounts = map[string]struct{}{}
	for _, vm := range in.NOS.VolumeMounts {
		existingMounts[strings.TrimSpace(vm.MountPath)] = struct{}{}
	}

	existingVolumes = map[string]struct{}{}
	for i := range in.Deployment.Spec.Template.Spec.Volumes {
		existingVolumes[in.Deployment.Spec.Template.Spec.Volumes[i].Name] = struct{}{}
	}

	return existingMounts, existingVolumes
}

func ensureSystemdTmpfsMounts(in *ApplyInput, existingMounts, existingVolumes map[string]struct{}) {
	addEmptyDir := func(volName, mountPath string, medium k8scorev1.StorageMedium) {
		volName = strings.TrimSpace(volName)

		mountPath = strings.TrimSpace(mountPath)
		if volName == "" || mountPath == "" {
			return
		}

		if _, ok := existingMounts[mountPath]; ok {
			return
		}

		if _, ok := existingVolumes[volName]; !ok {
			in.Deployment.Spec.Template.Spec.Volumes = append(
				in.Deployment.Spec.Template.Spec.Volumes,
				k8scorev1.Volume{
					Name: volName,
					VolumeSource: k8scorev1.VolumeSource{
						EmptyDir: &k8scorev1.EmptyDirVolumeSource{
							Medium: medium,
						},
					},
				},
			)
			existingVolumes[volName] = struct{}{}
		}

		in.NOS.VolumeMounts = append(
			in.NOS.VolumeMounts,
			k8scorev1.VolumeMount{
				Name:      volName,
				MountPath: mountPath,
			},
		)
		existingMounts[mountPath] = struct{}{}
	}

	addEmptyDir("systemd-run", "/run", k8scorev1.StorageMediumMemory)
	addEmptyDir("systemd-runlock", "/run/lock", k8scorev1.StorageMediumMemory)
	addEmptyDir("systemd-tmp", "/tmp", k8scorev1.StorageMediumMemory)
}

func mountCEOSStartupConfig(
	in *ApplyInput,
	existingMounts map[string]struct{},
	startupConfigPath string,
) bool {
	if startupConfigPath == "" {
		return false
	}

	if _, ok := existingMounts["/mnt/flash/startup-config"]; ok {
		return true
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
				MountPath: "/mnt/flash/startup-config",
				SubPath:   f.ConfigMapPath,
			},
		)
		existingMounts["/mnt/flash/startup-config"] = struct{}{}

		return true
	}

	return false
}

func buildExistingEnv(in *ApplyInput) map[string]string {
	existingEnv := map[string]string{}
	for _, e := range in.NOS.Env {
		existingEnv[strings.TrimSpace(e.Name)] = e.Value
	}

	return existingEnv
}

func applyCEOSEnv(in *ApplyInput, existingEnv map[string]string) {
	ceosEnv := map[string]string{
		"CEOS":                                "1",
		"EOS_PLATFORM":                        "ceoslab",
		"container":                           "docker",
		"ETBA":                                "1",
		"SKIP_ZEROTOUCH_BARRIER_IN_SYSDBINIT": "1",
		"MAPETH0":                             "1",
		"MGMT_INTF":                           "eth0",
	}

	intfType := strings.TrimSpace(existingEnv["INTFTYPE"])
	if intfType == "" {
		intfType = "eth"
	}

	ceosEnv["INTFTYPE"] = intfType

	// In native mode we expect management reachability to come from the Kubernetes pod network.
	// Passing CLAB_MGMT_VRF through to cEOS can result in the pod network interface being
	// repurposed and losing the pod IP. Remove it to preserve Kubernetes networking for SSH.
	delete(existingEnv, "CLAB_MGMT_VRF")

	in.NOS.Env = slices.DeleteFunc(in.NOS.Env, func(ev k8scorev1.EnvVar) bool {
		return strings.TrimSpace(ev.Name) == "CLAB_MGMT_VRF"
	})

	upsertEnv := func(key, value string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}

		existingEnv[key] = value

		for i := range in.NOS.Env {
			if in.NOS.Env[i].Name == key {
				in.NOS.Env[i].Value = value

				return
			}
		}

		in.NOS.Env = append(in.NOS.Env, k8scorev1.EnvVar{Name: key, Value: value})
	}

	for k, v := range ceosEnv {
		upsertEnv(k, v)
	}

	slices.SortFunc(
		in.NOS.Env,
		func(a, b k8scorev1.EnvVar) int { return strings.Compare(a.Name, b.Name) },
	)
}

func setCEOSInitCommand(in *ApplyInput, existingEnv map[string]string) {
	setenvKeys := make([]string, 0, len(existingEnv))
	for k := range existingEnv {
		switch k {
		case "CEOS",
			"EOS_PLATFORM",
			"container",
			"ETBA",
			"SKIP_ZEROTOUCH_BARRIER_IN_SYSDBINIT",
			"MAPETH0",
			"MGMT_INTF",
			"INTFTYPE":
			setenvKeys = append(setenvKeys, k)
		}
	}

	slices.Sort(setenvKeys)

	var cmd strings.Builder
	cmd.WriteString("exec /sbin/init ")

	for _, k := range setenvKeys {
		cmd.WriteString("systemd.setenv=")
		cmd.WriteString(k)
		cmd.WriteString("=")
		cmd.WriteString(existingEnv[k])
		cmd.WriteString(" ")
	}

	in.NOS.Command = []string{"bash", "-c", strings.TrimSpace(cmd.String())}
}
