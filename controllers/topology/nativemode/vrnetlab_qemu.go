package nativemode

import (
	"strconv"
	"strings"

	clabernetesutilkubernetes "github.com/srl-labs/clabernetes/util/kubernetes"
	k8scorev1 "k8s.io/api/core/v1"
)

func applyVrnetlabQemuNative(in *ApplyInput) {
	if in == nil || in.NOS == nil || in.NodeDef == nil {
		return
	}

	// Many vrnetlab QEMU-based images use CLAB_INTFS to decide how many
	// dataplane interfaces (eth1..ethN) to wire into QEMU.
	//
	// In clabernetes native mode we do not run containerlab inside docker,
	// so we must provide equivalent metadata ourselves.
	computeClabIntfs := func(nodeName string) int {
		nodeName = strings.TrimSpace(nodeName)
		if nodeName == "" {
			return 0
		}

		maxEthIndex := 0
		for _, link := range in.Links {
			if link == nil {
				continue
			}
			for _, ep := range link.Endpoints {
				ep = strings.TrimSpace(ep)
				if ep == "" {
					continue
				}

				parts := strings.SplitN(ep, ":", 2)
				if len(parts) != 2 {
					continue
				}

				if strings.TrimSpace(parts[0]) != nodeName {
					continue
				}

				ifName := strings.TrimSpace(parts[1])
				if ifName == "" {
					continue
				}

				ifName = strings.SplitN(ifName, ".", 2)[0]
				if !strings.HasPrefix(ifName, "eth") {
					continue
				}

				idx, err := strconv.Atoi(strings.TrimPrefix(ifName, "eth"))
				if err != nil || idx <= 0 {
					continue
				}

				if idx > maxEthIndex {
					maxEthIndex = idx
				}
			}
		}

		return maxEthIndex
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

	// Ensure that vrnetlab's startup-config is available in the NOS container.
	//
	// In clabernetes native mode, FilesFromConfigMap volumes are mounted into the launcher/setup
	// containers by default. Many vrnetlab images expect the startup config to be mounted into the
	// NOS container at a fixed path (`/config/startup-config.cfg`). Without this mount, vrnetlab
	// will boot with only its internal bootstrap config and any netlab-generated configuration
	// (routed interfaces, protocols, etc.) will never be applied.
	startupConfigPath := strings.TrimSpace(in.NodeDef.StartupConfig)
	if startupConfigPath != "" {
		const vrnetlabStartupConfigMount = "/config/startup-config.cfg"

		existingMounts, _ := collectExistingMountsAndVolumes(in)
		if _, ok := existingMounts[vrnetlabStartupConfigMount]; !ok {
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
				break
			}
		}
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

	// Provide the dataplane interface count for vrnetlab.
	//
	// We compute this from the link endpoints present in the topology (eth1..ethN).
	// This value is used by vrnetlab to wire QEMU NICs to the container's eth1..ethN.
	if n := computeClabIntfs(in.NodeName); n > 0 {
		upsertEnv("CLAB_INTFS", strconv.Itoa(n))
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
