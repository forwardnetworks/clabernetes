package nativemode

import (
	"fmt"
	"strings"

	k8scorev1 "k8s.io/api/core/v1"
)

const (
	bindSplitNParts = 3
	bindMinParts    = 2
)

//nolint:gocyclo // this is a linear translation of containerlab bind syntax to k8s volumes
func applyBindMounts(in *ApplyInput) {
	if in == nil || in.NodeDef == nil || len(in.NodeDef.Binds) == 0 {
		return
	}

	// Best-effort support for bind mounts in native mode.
	//
	// In "classic" (non-native) mode, containerlab/Docker handles common node requirements for
	// network OSes. In native mode, the NOS runs as a Kubernetes container and relies on k8s
	// volume mounts. We translate *absolute* host bind mounts into HostPath volumes.
	existingMounts := map[string]struct{}{}
	for _, vm := range in.NOS.VolumeMounts {
		existingMounts[strings.TrimSpace(vm.MountPath)] = struct{}{}
	}

	existingVolumes := map[string]struct{}{}
	for i := range in.Deployment.Spec.Template.Spec.Volumes {
		existingVolumes[in.Deployment.Spec.Template.Spec.Volumes[i].Name] = struct{}{}
	}

	for idx, bind := range in.NodeDef.Binds {
		bind = strings.TrimSpace(bind)
		if bind == "" {
			continue
		}

		parts := strings.SplitN(bind, ":", bindSplitNParts)
		if len(parts) < bindMinParts {
			continue
		}

		hostPath := strings.TrimSpace(parts[0])

		containerPath := strings.TrimSpace(parts[1])
		if hostPath == "" || containerPath == "" {
			continue
		}

		// Only translate absolute host paths; relative binds are typically "node_files" and are
		// handled via other clabernetes mechanisms.
		if !strings.HasPrefix(hostPath, "/") {
			continue
		}

		if _, ok := existingMounts[containerPath]; ok {
			continue
		}

		readOnly := false

		if len(parts) == bindSplitNParts {
			opts := strings.TrimSpace(parts[2])
			if strings.Contains(opts, "ro") {
				readOnly = true
			}
		}

		volName := fmt.Sprintf("bind-%d", idx)

		for i := 2; ; i++ {
			if _, ok := existingVolumes[volName]; !ok {
				break
			}

			volName = fmt.Sprintf("bind-%d-%d", idx, i)
		}

		existingVolumes[volName] = struct{}{}
		existingMounts[containerPath] = struct{}{}

		in.Deployment.Spec.Template.Spec.Volumes = append(
			in.Deployment.Spec.Template.Spec.Volumes,
			k8scorev1.Volume{
				Name: volName,
				VolumeSource: k8scorev1.VolumeSource{
					HostPath: &k8scorev1.HostPathVolumeSource{
						Path: hostPath,
					},
				},
			},
		)
		in.NOS.VolumeMounts = append(
			in.NOS.VolumeMounts,
			k8scorev1.VolumeMount{
				Name:      volName,
				ReadOnly:  readOnly,
				MountPath: containerPath,
			},
		)
	}
}
