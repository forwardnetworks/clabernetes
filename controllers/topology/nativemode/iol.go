package nativemode

import (
	_ "embed"
	"fmt"
	"slices"
	"strconv"
	"strings"

	claberneteslogging "github.com/srl-labs/clabernetes/logging"
	clabernetesutilkubernetes "github.com/srl-labs/clabernetes/util/kubernetes"
	k8scorev1 "k8s.io/api/core/v1"
)

//go:embed scripts/iol-bootstrap.sh
var iolBootstrapScript string

const (
	iolPIDHashShift   = 16
	iolPIDMod         = 1023
	iolPIDMin         = 1
	iolLinkIfacesCap  = 8
	splitNParts       = 2
	linuxIfNameMaxLen = 15
)

func applyCiscoIOL(log claberneteslogging.Instance, in *ApplyInput) {
	if in == nil {
		return
	}

	existingMounts, existingVolumes := collectExistingMountsAndVolumes(in)
	ensureIOLRuntimeVolume(in, existingMounts, existingVolumes)

	pid, nvramName := computeIOLPIDAndNVRAM(in)
	mountIOLNetlabInputs(in, existingMounts)

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

	upsertEnv("SKYFORGE_NODE_NAME", in.NodeName)
	upsertEnv("IOL_PID", strconv.Itoa(pid))
	upsertEnv("SKYFORGE_IOL_NVRAM", nvramName)
	upsertEnv("SKYFORGE_IOL_LINK_IFACES", strings.Join(collectIOLLinkIfaces(in), ","))

	slices.SortFunc(
		in.NOS.Env,
		func(a, b k8scorev1.EnvVar) int { return strings.Compare(a.Name, b.Name) },
	)

	in.NOS.Command = []string{"bash", "-lc", strings.TrimSpace(iolBootstrapScript)}
	if strings.TrimSpace(iolBootstrapScript) == "" {
		log.Warn("nativemode: iol bootstrap script is empty")
	}
}

func ensureIOLRuntimeVolume(in *ApplyInput, existingMounts, existingVolumes map[string]struct{}) {
	const runtimeVol = "vrnetlab-runtime"

	if _, ok := existingVolumes[runtimeVol]; !ok {
		in.Deployment.Spec.Template.Spec.Volumes = append(
			in.Deployment.Spec.Template.Spec.Volumes,
			k8scorev1.Volume{
				Name: runtimeVol,
				VolumeSource: k8scorev1.VolumeSource{
					EmptyDir: &k8scorev1.EmptyDirVolumeSource{},
				},
			},
		)
		existingVolumes[runtimeVol] = struct{}{}
	}

	if _, ok := existingMounts["/vrnetlab"]; ok {
		return
	}

	in.NOS.VolumeMounts = append(
		in.NOS.VolumeMounts,
		k8scorev1.VolumeMount{
			Name:      runtimeVol,
			ReadOnly:  false,
			MountPath: "/vrnetlab",
		},
	)
	existingMounts["/vrnetlab"] = struct{}{}
}

func computeIOLPIDAndNVRAM(in *ApplyInput) (pid int, nvramName string) {
	const (
		fnv32Offset = 2166136261
		fnv32Prime  = 16777619
	)

	seed := strings.TrimSpace(in.OwningTopology.Name) + ":" + strings.TrimSpace(in.NodeName)

	h := uint32(fnv32Offset)
	for i := range len(seed) {
		h ^= uint32(seed[i])
		h *= fnv32Prime
	}

	pid = int((h>>iolPIDHashShift)%iolPIDMod) + iolPIDMin
	nvramName = fmt.Sprintf("nvram_%05d", pid)

	return pid, nvramName
}

func mountIOLNetlabInputs(in *ApplyInput, existingMounts map[string]struct{}) {
	mountNetlabInitialConfig(in, existingMounts)
	mountSkyforgeC9sArtifacts(in, existingMounts)
}

func mountNetlabInitialConfig(in *ApplyInput, existingMounts map[string]struct{}) {
	if _, ok := existingMounts["/netlab/initial.cfg"]; ok {
		return
	}

	for _, f := range in.FilesFromConfigMap {
		if strings.TrimSpace(f.ConfigMapName) == "" || strings.TrimSpace(f.ConfigMapPath) == "" {
			continue
		}

		if strings.TrimSpace(f.ConfigMapPath) != "initial" {
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
				MountPath: "/netlab/initial.cfg",
				SubPath:   f.ConfigMapPath,
			},
		)
		existingMounts["/netlab/initial.cfg"] = struct{}{}

		return
	}
}

func mountSkyforgeC9sArtifacts(in *ApplyInput, existingMounts map[string]struct{}) {
	for _, f := range in.FilesFromConfigMap {
		if strings.TrimSpace(f.ConfigMapName) == "" || strings.TrimSpace(f.ConfigMapPath) == "" {
			continue
		}

		mountPath := strings.TrimSpace(f.FilePath)
		if mountPath == "" || !strings.HasPrefix(mountPath, "/tmp/skyforge-c9s/") {
			continue
		}

		if _, ok := existingMounts[mountPath]; ok {
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
				MountPath: mountPath,
				SubPath:   f.ConfigMapPath,
			},
		)

		existingMounts[mountPath] = struct{}{}
	}
}

func collectIOLLinkIfaces(in *ApplyInput) []string {
	linkIfacesSet := map[string]struct{}{}
	linkIfaces := make([]string, 0, iolLinkIfacesCap)

	for _, l := range in.Links {
		if l == nil || len(l.Endpoints) != 2 {
			continue
		}

		for _, ep := range l.Endpoints {
			parts := strings.SplitN(strings.TrimSpace(ep), ":", splitNParts)
			if len(parts) != splitNParts || parts[0] != in.NodeName {
				continue
			}

			ifName := sanitizeLinuxIfName(parts[1])
			if ifName == "" {
				continue
			}

			if _, ok := linkIfacesSet[ifName]; ok {
				continue
			}

			linkIfacesSet[ifName] = struct{}{}
			linkIfaces = append(linkIfaces, ifName)
		}
	}

	slices.Sort(linkIfaces)

	return linkIfaces
}

func sanitizeLinuxIfName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' ||
			r == '-' {
			out = append(out, r)

			continue
		}

		out = append(out, '_')
	}

	if len(out) > linuxIfNameMaxLen {
		out = out[:linuxIfNameMaxLen]
	}

	return strings.ToLower(string(out))
}
