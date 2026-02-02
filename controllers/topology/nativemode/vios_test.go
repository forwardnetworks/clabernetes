package nativemode

import (
	"testing"

	clabernetesapisv1alpha1 "github.com/srl-labs/clabernetes/apis/v1alpha1"
	claberneteslogging "github.com/srl-labs/clabernetes/logging"
	clabernetesutilcontainerlab "github.com/srl-labs/clabernetes/util/containerlab"
	k8sappsv1 "k8s.io/api/apps/v1"
	k8scorev1 "k8s.io/api/core/v1"
)

func TestApplyCiscoVIOS_MountsStartupConfigToConfigDir(t *testing.T) {
	t.Parallel()

	log := &claberneteslogging.FakeInstance{}

	in := &ApplyInput{
		OwningTopology: &clabernetesapisv1alpha1.Topology{},
		NodeName:       "r1",
		NodeDef: &clabernetesutilcontainerlab.NodeDefinition{
			Kind:          "cisco_vios",
			StartupConfig: "configs/r1.cfg",
		},
		FilesFromConfigMap: []clabernetesapisv1alpha1.FileFromConfigMap{
			{
				ConfigMapName: "cm-r1",
				ConfigMapPath: "r1.cfg",
				FilePath:      "configs/r1.cfg",
			},
		},
		Deployment: &k8sappsv1.Deployment{},
		NOS:        &k8scorev1.Container{},
	}

	ApplyNativeModeOverrides(log, in)

	found := false
	for _, vm := range in.NOS.VolumeMounts {
		if vm.MountPath == "/config/startup-config.cfg" {
			found = true
			if vm.SubPath != "r1.cfg" {
				t.Fatalf("expected subPath r1.cfg, got %q", vm.SubPath)
			}
			break
		}
	}
	if !found {
		t.Fatalf("expected /config/startup-config.cfg volume mount to be present")
	}
}
