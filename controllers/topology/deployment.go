package topology

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	clabernetesapisv1alpha1 "github.com/srl-labs/clabernetes/apis/v1alpha1"
	clabernetesconfig "github.com/srl-labs/clabernetes/config"
	clabernetesconstants "github.com/srl-labs/clabernetes/constants"
	claberneteserrors "github.com/srl-labs/clabernetes/errors"
	claberneteslogging "github.com/srl-labs/clabernetes/logging"
	clabernetesutil "github.com/srl-labs/clabernetes/util"
	clabernetesutilcontainerlab "github.com/srl-labs/clabernetes/util/containerlab"
	clabernetesutilkubernetes "github.com/srl-labs/clabernetes/util/kubernetes"
	k8sappsv1 "k8s.io/api/apps/v1"
	k8scorev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachinerytypes "k8s.io/apimachinery/pkg/types"
)

const (
	probeInitialDelay                   = 60
	probePeriodSeconds                  = 20
	probeReadinessFailureThreshold      = 3
	probeDefaultStartupFailureThreshold = 40
)

func sanitizeLinuxIfName(raw string) string {
	// Linux interface names must be <= 15 bytes and cannot contain '/'.
	s := strings.TrimSpace(raw)
	if s == "" {
		return "link"
	}
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ":", "-")
	s = strings.ReplaceAll(s, " ", "-")

	// Keep only [A-Za-z0-9_.-]
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			b = append(b, c)
		case c >= 'A' && c <= 'Z':
			b = append(b, c)
		case c >= '0' && c <= '9':
			b = append(b, c)
		case c == '_' || c == '-' || c == '.':
			b = append(b, c)
		default:
			// drop
		}
	}
	s = string(b)
	if s == "" {
		return "link"
	}
	if len(s) <= 15 {
		return s
	}

	sum := sha1.Sum([]byte(s))           //nolint:gosec // non-crypto identifier
	suffix := fmt.Sprintf("%x", sum[:3]) // 6 chars
	prefixLen := 15 - 1 - len(suffix)
	if prefixLen < 1 {
		return suffix[:15]
	}
	return s[:prefixLen] + "-" + suffix
}

// DeploymentReconciler is a subcomponent of the "TopologyReconciler" but is exposed for testing
// purposes. This is the component responsible for rendering/validating deployments for a
// clabernetes topology resource.
type DeploymentReconciler struct {
	log                 claberneteslogging.Instance
	managerAppName      string
	managerNamespace    string
	criKind             string
	configManagerGetter clabernetesconfig.ManagerGetterFunc
}

// NewDeploymentReconciler returns an instance of DeploymentReconciler.
func NewDeploymentReconciler(
	log claberneteslogging.Instance,
	managerAppName,
	managerNamespace,
	criKind string,
	configManagerGetter clabernetesconfig.ManagerGetterFunc,
) *DeploymentReconciler {
	return &DeploymentReconciler{
		log:                 log,
		managerAppName:      managerAppName,
		managerNamespace:    managerNamespace,
		criKind:             criKind,
		configManagerGetter: configManagerGetter,
	}
}

// Resolve accepts a mapping of clabernetes configs and a list of deployments that are -- by owner
// reference and/or labels -- associated with the topology. It returns a ObjectDiffer object
// that contains the missing, extra, and current deployments for the topology.
func (r *DeploymentReconciler) Resolve(
	ownedDeployments *k8sappsv1.DeploymentList,
	clabernetesConfigs map[string]*clabernetesutilcontainerlab.Config,
	_ *clabernetesapisv1alpha1.Topology,
) (*clabernetesutil.ObjectDiffer[*k8sappsv1.Deployment], error) {
	deployments := &clabernetesutil.ObjectDiffer[*k8sappsv1.Deployment]{
		Current: map[string]*k8sappsv1.Deployment{},
	}

	for i := range ownedDeployments.Items {
		labels := ownedDeployments.Items[i].Labels

		if labels == nil {
			return nil, fmt.Errorf(
				"%w: labels are nil, but we expect to see topology owner label here",
				claberneteserrors.ErrInvalidData,
			)
		}

		nodeName, ok := labels[clabernetesconstants.LabelTopologyNode]
		if !ok || nodeName == "" {
			return nil, fmt.Errorf(
				"%w: topology node label is missing or empty",
				claberneteserrors.ErrInvalidData,
			)
		}

		deployments.Current[nodeName] = &ownedDeployments.Items[i]
	}

	allNodes := make([]string, len(clabernetesConfigs))

	var nodeIdx int

	for nodeName := range clabernetesConfigs {
		allNodes[nodeIdx] = nodeName

		nodeIdx++
	}

	deployments.SetMissing(allNodes)
	deployments.SetExtra(allNodes)

	return deployments, nil
}

// Render accepts the owning topology a mapping of clabernetes sub-topology configs and a node name
// and renders the final deployment for this node.
func (r *DeploymentReconciler) Render(
	owningTopology *clabernetesapisv1alpha1.Topology,
	clabernetesConfigs map[string]*clabernetesutilcontainerlab.Config,
	nodeName string,
) *k8sappsv1.Deployment {
	owningTopologyName := owningTopology.GetName()

	deploymentName := fmt.Sprintf("%s-%s", owningTopologyName, nodeName)

	if ResolveTopologyRemovePrefix(owningTopology) {
		deploymentName = nodeName
	}

	configVolumeName := fmt.Sprintf("%s-config", owningTopologyName)

	deployment := r.renderDeploymentBase(
		deploymentName,
		owningTopology.GetNamespace(),
		owningTopologyName,
		nodeName,
		owningTopology,
	)

	r.renderDeploymentScheduling(
		deployment,
		owningTopology,
	)

	volumeMountsFromCommonSpec := r.renderDeploymentVolumes(
		deployment,
		nodeName,
		configVolumeName,
		owningTopologyName,
		owningTopology,
	)

	r.renderDeploymentContainer(
		deployment,
		nodeName,
		configVolumeName,
		volumeMountsFromCommonSpec,
		owningTopology,
		clabernetesConfigs,
	)

	r.renderDeploymentNative(
		deployment,
		nodeName,
		configVolumeName,
		volumeMountsFromCommonSpec,
		owningTopology,
		clabernetesConfigs,
	)

	r.renderDeploymentMultus(
		deployment,
		owningTopology,
		nodeName,
		clabernetesConfigs,
	)

	r.renderDeploymentVrnetlabMgmt(
		deployment,
		nodeName,
		clabernetesConfigs,
	)

	r.renderDeploymentContainerEnv(
		deployment,
		nodeName,
		owningTopologyName,
		owningTopology,
		clabernetesConfigs,
	)

	r.renderDeploymentContainerResources(
		deployment,
		nodeName,
		owningTopology,
		clabernetesConfigs,
	)

	r.renderDeploymentNodeSelectors(
		deployment,
		nodeName,
		owningTopology,
		clabernetesConfigs,
	)

	r.renderDeploymentContainerPrivileges(
		deployment,
		nodeName,
		owningTopology,
	)

	r.renderDeploymentContainerStatus(
		deployment,
		nodeName,
		owningTopology,
	)

	r.renderDeploymentDevices(
		deployment,
		owningTopology,
	)

	r.renderDeploymentPersistence(
		deployment,
		nodeName,
		owningTopologyName,
		owningTopology,
	)

	return deployment
}

// RenderAll accepts the owning topology a mapping of clabernetes sub-topology configs and a
// list of node names and renders the final deployments for the given nodes.
func (r *DeploymentReconciler) RenderAll(
	owningTopology *clabernetesapisv1alpha1.Topology,
	clabernetesConfigs map[string]*clabernetesutilcontainerlab.Config,
	nodeNames []string,
) []*k8sappsv1.Deployment {
	deployments := make([]*k8sappsv1.Deployment, len(nodeNames))

	for idx, nodeName := range nodeNames {
		deployments[idx] = r.Render(
			owningTopology,
			clabernetesConfigs,
			nodeName,
		)
	}

	return deployments
}

// Conforms checks if the existingDeployment conforms with the renderedDeployment.
func (r *DeploymentReconciler) Conforms( //nolint: gocyclo
	existingDeployment,
	renderedDeployment *k8sappsv1.Deployment,
	expectedOwnerUID apimachinerytypes.UID,
) bool {
	if !reflect.DeepEqual(existingDeployment.Spec.Replicas, renderedDeployment.Spec.Replicas) {
		return false
	}

	if !reflect.DeepEqual(existingDeployment.Spec.Selector, renderedDeployment.Spec.Selector) {
		return false
	}

	if renderedDeployment.Spec.Template.Spec.Hostname !=
		existingDeployment.Spec.Template.Spec.Hostname {
		return false
	}

	if !clabernetesutilkubernetes.ExistingMapStringStringContainsAllExpectedKeyValues(
		existingDeployment.Spec.Template.Spec.NodeSelector,
		renderedDeployment.Spec.Template.Spec.NodeSelector,
	) {
		return false
	}

	if !reflect.DeepEqual(
		existingDeployment.Spec.Template.Spec.Tolerations,
		renderedDeployment.Spec.Template.Spec.Tolerations,
	) {
		return false
	}

	if !reflect.DeepEqual(
		existingDeployment.Spec.Template.Spec.Volumes,
		renderedDeployment.Spec.Template.Spec.Volumes,
	) {
		return false
	}

	if !clabernetesutilkubernetes.ContainersEqual(
		existingDeployment.Spec.Template.Spec.Containers,
		renderedDeployment.Spec.Template.Spec.Containers,
	) {
		return false
	}

	if !reflect.DeepEqual(
		existingDeployment.Spec.Template.Spec.ServiceAccountName,
		renderedDeployment.Spec.Template.Spec.ServiceAccountName,
	) {
		return false
	}

	if !reflect.DeepEqual(
		existingDeployment.Spec.Template.Spec.RestartPolicy,
		renderedDeployment.Spec.Template.Spec.RestartPolicy,
	) {
		return false
	}

	if !clabernetesutilkubernetes.ExistingMapStringStringContainsAllExpectedKeyValues(
		existingDeployment.ObjectMeta.Annotations,
		renderedDeployment.ObjectMeta.Annotations,
	) {
		return false
	}

	if !clabernetesutilkubernetes.ExistingMapStringStringContainsAllExpectedKeyValues(
		existingDeployment.ObjectMeta.Labels,
		renderedDeployment.ObjectMeta.Labels,
	) {
		return false
	}

	if !clabernetesutilkubernetes.ExistingMapStringStringContainsAllExpectedKeyValues(
		existingDeployment.Spec.Template.ObjectMeta.Annotations,
		renderedDeployment.Spec.Template.ObjectMeta.Annotations,
	) {
		return false
	}

	if !clabernetesutilkubernetes.ExistingMapStringStringContainsAllExpectedKeyValues(
		existingDeployment.Spec.Template.ObjectMeta.Labels,
		renderedDeployment.Spec.Template.ObjectMeta.Labels,
	) {
		return false
	}

	if len(existingDeployment.ObjectMeta.OwnerReferences) != 1 {
		// we should have only one owner reference, the owning topology
		return false
	}

	if existingDeployment.ObjectMeta.OwnerReferences[0].UID != expectedOwnerUID {
		// owner ref uid is not us
		return false
	}

	return true
}

// DetermineNodesNeedingRestart accepts reconcile data (which contains the previous and current
// rendered sub-topologies) and updates the reconcile data NodesNeedingReboot set with each node
// that needs restarting due to configuration changes.
func (r *DeploymentReconciler) DetermineNodesNeedingRestart(
	reconcileData *ReconcileData,
) {
	// When the rendered containerlab config changes, we have to restart nodes so
	// the launcher realizes the updated config. Historically we tried to diff the
	// unmarshaled config structs node-by-node, but because the "previous" configs
	// are round-tripped through YAML (Topology status), benign serialization
	// differences can cause endless restart loops.
	//
	// The config hash already captures real changes (and is stable), so use it as
	// the gate for restarts and restart all existing nodes when it changes.
	if reconcileData.PreviousHashes.Config == reconcileData.ResolvedHashes.Config {
		return
	}

	for nodeName := range reconcileData.ResolvedConfigs {
		_, nodeExistedBefore := reconcileData.PreviousConfigs[nodeName]
		if !nodeExistedBefore {
			continue
		}

		reconcileData.NodesNeedingReboot.Add(nodeName)
	}
}

func (r *DeploymentReconciler) renderDeploymentBase(
	name,
	namespace,
	owningTopologyName,
	nodeName string,
	owningTopology *clabernetesapisv1alpha1.Topology,
) *k8sappsv1.Deployment {
	annotations, globalLabels := r.configManagerGetter().GetAllMetadata()

	selectorLabels := map[string]string{
		clabernetesconstants.LabelKubernetesName: name,
		clabernetesconstants.LabelApp:            clabernetesconstants.Clabernetes,
		clabernetesconstants.LabelName:           name,
		clabernetesconstants.LabelTopologyOwner:  owningTopologyName,
		clabernetesconstants.LabelTopologyNode:   nodeName,
	}

	labels := map[string]string{}

	for k, v := range selectorLabels {
		labels[k] = v
	}

	for k, v := range globalLabels {
		labels[k] = v
	}

	deployment := &k8sappsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
			Labels:      labels,
		},
		Spec: k8sappsv1.DeploymentSpec{
			Replicas:             clabernetesutil.ToPointer(int32(1)),
			RevisionHistoryLimit: clabernetesutil.ToPointer(int32(0)),
			Strategy: k8sappsv1.DeploymentStrategy{
				// in our case there is no (current?) need for more gracefully updating our
				// deployments, so just yolo recreate them instead...
				Type:          k8sappsv1.RecreateDeploymentStrategyType,
				RollingUpdate: nil,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Template: k8scorev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: annotations,
					Labels:      labels,
				},
				Spec: k8scorev1.PodSpec{
					InitContainers:     []k8scorev1.Container{},
					Containers:         []k8scorev1.Container{},
					RestartPolicy:      "Always",
					ServiceAccountName: launcherServiceAccountName(),
					Volumes:            []k8scorev1.Volume{},
					Hostname:           nodeName,
				},
			},
		},
	}

	if ResolveNativeMode(owningTopology) {
		deployment.Spec.Template.Spec.ShareProcessNamespace = clabernetesutil.ToPointer(true)
	}

	if ResolveHostNetwork(owningTopology) {
		deployment.Spec.Template.Spec.HostNetwork = true
	}

	return deployment
}

func (r *DeploymentReconciler) renderDeploymentScheduling(
	deployment *k8sappsv1.Deployment,
	owningTopology *clabernetesapisv1alpha1.Topology,
) {
	tolerations := owningTopology.Spec.Deployment.Scheduling.Tolerations

	deployment.Spec.Template.Spec.Tolerations = tolerations

	// Nothing else to do for scheduling here; node placement is controlled by Topology scheduling
	// config and cluster policies.
}

func (r *DeploymentReconciler) renderDeploymentVolumes( //nolint:funlen
	deployment *k8sappsv1.Deployment,
	nodeName,
	configVolumeName,
	owningTopologyName string,
	owningTopology *clabernetesapisv1alpha1.Topology,
) []k8scorev1.VolumeMount {
	volumes := []k8scorev1.Volume{
		{
			Name: configVolumeName,
			VolumeSource: k8scorev1.VolumeSource{
				ConfigMap: &k8scorev1.ConfigMapVolumeSource{
					LocalObjectReference: k8scorev1.LocalObjectReference{
						Name: owningTopologyName,
					},
					DefaultMode: clabernetesutil.ToPointer(
						int32(clabernetesconstants.PermissionsEveryoneReadWriteOwnerExecute),
					),
				},
			},
		},
		{
			Name: "docker",
			VolumeSource: k8scorev1.VolumeSource{
				EmptyDir: &k8scorev1.EmptyDirVolumeSource{},
			},
		},
	}

	volumeMountsFromCommonSpec := make([]k8scorev1.VolumeMount, 0)

	criPath, criSubPath := r.renderDeploymentVolumesGetCRISockPath(owningTopology)

	if criPath != "" && criSubPath != "" {
		volumes = append(
			volumes,
			k8scorev1.Volume{
				Name: "cri-sock",
				VolumeSource: k8scorev1.VolumeSource{
					HostPath: &k8scorev1.HostPathVolumeSource{
						Path: criPath,
						Type: clabernetesutil.ToPointer(k8scorev1.HostPathType("")),
					},
				},
			},
		)

		volumeMountsFromCommonSpec = append(
			volumeMountsFromCommonSpec,
			k8scorev1.VolumeMount{
				Name:     "cri-sock",
				ReadOnly: true,
				MountPath: fmt.Sprintf(
					"%s/%s",
					clabernetesconstants.LauncherCRISockPath,
					criSubPath,
				),
				SubPath: criSubPath,
			},
		)
	}

	dockerDaemonConfigSecret := owningTopology.Spec.ImagePull.DockerDaemonConfig
	if dockerDaemonConfigSecret == "" {
		dockerDaemonConfigSecret = r.configManagerGetter().GetDockerDaemonConfig()
	}

	if dockerDaemonConfigSecret != "" {
		volumes = append(
			volumes,
			k8scorev1.Volume{
				Name: "docker-daemon-config",
				VolumeSource: k8scorev1.VolumeSource{
					Secret: &k8scorev1.SecretVolumeSource{
						SecretName: dockerDaemonConfigSecret,
						DefaultMode: clabernetesutil.ToPointer(
							int32(clabernetesconstants.PermissionsEveryoneReadWriteOwnerExecute),
						),
					},
				},
			},
		)

		volumeMountsFromCommonSpec = append(
			volumeMountsFromCommonSpec,
			k8scorev1.VolumeMount{
				Name:      "docker-daemon-config",
				ReadOnly:  true,
				MountPath: "/etc/docker",
			},
		)
	}

	dockerConfigSecret := owningTopology.Spec.ImagePull.DockerConfig
	if dockerConfigSecret == "" {
		dockerConfigSecret = r.configManagerGetter().GetDockerConfig()
	}

	if dockerConfigSecret != "" {
		volumes = append(
			volumes,
			k8scorev1.Volume{
				Name: "docker-config",
				VolumeSource: k8scorev1.VolumeSource{
					Secret: &k8scorev1.SecretVolumeSource{
						SecretName: dockerConfigSecret,
						DefaultMode: clabernetesutil.ToPointer(
							int32(clabernetesconstants.PermissionsEveryoneReadWriteOwnerExecute),
						),
					},
				},
			},
		)

		volumeMountsFromCommonSpec = append(
			volumeMountsFromCommonSpec,
			k8scorev1.VolumeMount{
				Name:      "docker-config",
				ReadOnly:  true,
				MountPath: "/root/.docker",
			},
		)
	}

	volumesFromConfigMaps := make([]clabernetesapisv1alpha1.FileFromConfigMap, 0)

	volumesFromConfigMaps = append(
		volumesFromConfigMaps,
		owningTopology.Spec.Deployment.FilesFromConfigMap[nodeName]...,
	)

	for _, podVolume := range volumesFromConfigMaps {
		volumeName := clabernetesutilkubernetes.EnforceDNSLabelConvention(
			clabernetesutilkubernetes.SafeConcatNameKubernetes(
				podVolume.ConfigMapName,
				podVolume.ConfigMapPath,
			),
		)

		var mode *int32

		switch podVolume.Mode {
		case clabernetesconstants.FileModeRead:
			mode = clabernetesutil.ToPointer(
				int32(clabernetesconstants.PermissionsEveryoneRead),
			)
		case clabernetesconstants.FileModeExecute:
			mode = clabernetesutil.ToPointer(
				int32(clabernetesconstants.PermissionsEveryoneReadExecute),
			)
		default:
			mode = nil
		}

		volumes = append(
			volumes,
			k8scorev1.Volume{
				Name: volumeName,
				VolumeSource: k8scorev1.VolumeSource{
					ConfigMap: &k8scorev1.ConfigMapVolumeSource{
						LocalObjectReference: k8scorev1.LocalObjectReference{
							Name: podVolume.ConfigMapName,
						},
						DefaultMode: mode,
					},
				},
			},
		)

		var mountPath string
		// mount relative paths under /clabernetes, and absolute paths as is
		if strings.HasPrefix(podVolume.FilePath, "/") {
			mountPath = podVolume.FilePath
		} else {
			mountPath = fmt.Sprintf("/clabernetes/%s", podVolume.FilePath)
		}

		volumeMount := k8scorev1.VolumeMount{
			Name:      volumeName,
			ReadOnly:  false,
			MountPath: mountPath,
			SubPath:   podVolume.ConfigMapPath,
		}

		volumeMountsFromCommonSpec = append(
			volumeMountsFromCommonSpec,
			volumeMount,
		)
	}

	deployment.Spec.Template.Spec.Volumes = volumes

	return volumeMountsFromCommonSpec
}

func (r *DeploymentReconciler) renderDeploymentVolumesGetCRISockPath(
	owningTopology *clabernetesapisv1alpha1.Topology,
) (path, subPath string) {
	if owningTopology.Spec.ImagePull.PullThroughOverride == clabernetesconstants.ImagePullThroughModeNever { //nolint:lll
		// obviously the topology is set to *never*, so nothing to do...
		return path, subPath
	}

	if owningTopology.Spec.ImagePull.PullThroughOverride == "" && r.configManagerGetter().
		GetImagePullThroughMode() == clabernetesconstants.ImagePullThroughModeNever {
		// our specific topology is setting is unset, so we default to the global value, if that
		// is never then we are obviously done here
		return path, subPath
	}

	criSockOverrideFullPath := r.configManagerGetter().GetImagePullCriSockOverride()
	if criSockOverrideFullPath != "" {
		path, subPath = filepath.Split(criSockOverrideFullPath)

		if path == "" {
			r.log.Warn(
				"image pull cri path override is set, but failed to parse path/subpath," +
					" will skip mounting cri sock",
			)

			return path, subPath
		}
	} else {
		switch r.criKind {
		case clabernetesconstants.KubernetesCRIContainerd:
			path = clabernetesconstants.KubernetesCRISockContainerdPath

			subPath = clabernetesconstants.KubernetesCRISockContainerd
		default:
			r.log.Warnf(
				"image pull through mode is auto or always but cri kind is not containerd!"+
					" got cri kind %q",
				r.criKind,
			)
		}
	}

	return path, subPath
}

func (r *DeploymentReconciler) renderDeploymentContainer(
	deployment *k8sappsv1.Deployment,
	nodeName,
	configVolumeName string,
	volumeMountsFromCommonSpec []k8scorev1.VolumeMount,
	owningTopology *clabernetesapisv1alpha1.Topology,
	clabernetesConfigs map[string]*clabernetesutilcontainerlab.Config,
) {
	nativeMode := ResolveNativeMode(owningTopology)

	image := owningTopology.Spec.Deployment.LauncherImage
	if image == "" {
		image = r.configManagerGetter().GetLauncherImage()
	}

	imagePullPolicy := owningTopology.Spec.Deployment.LauncherImagePullPolicy
	if imagePullPolicy == "" {
		imagePullPolicy = r.configManagerGetter().GetLauncherImagePullPolicy()
	}

	launcherContainer := k8scorev1.Container{
		Name:       nodeName,
		WorkingDir: "/clabernetes",
		Image:      image,
		Command:    []string{"/clabernetes/manager", "launch"},
		Ports: []k8scorev1.ContainerPort{
			{
				Name:          clabernetesconstants.ConnectivityVXLAN,
				ContainerPort: clabernetesconstants.VXLANServicePort,
				Protocol:      clabernetesconstants.UDP,
			},
			{
				Name:          clabernetesconstants.ConnectivitySlurpeeth,
				ContainerPort: clabernetesconstants.SlurpeethServicePort,
				Protocol:      clabernetesconstants.TCP,
			},
		},
		VolumeMounts: []k8scorev1.VolumeMount{
			{
				Name:      configVolumeName,
				ReadOnly:  true,
				MountPath: "/clabernetes/topo.clab.yaml",
				SubPath:   nodeName,
			},
			{
				Name:      configVolumeName,
				ReadOnly:  true,
				MountPath: "/clabernetes/files-from-url.yaml",
				SubPath:   fmt.Sprintf("%s-files-from-url", nodeName),
			},
			{
				Name:      configVolumeName,
				ReadOnly:  true,
				MountPath: "/clabernetes/configured-pull-secrets.yaml",
				SubPath:   "configured-pull-secrets",
			},
			{
				Name:      "docker",
				ReadOnly:  false,
				MountPath: "/var/lib/docker",
			},
		},
		TerminationMessagePath:   "/dev/termination-log",
		TerminationMessagePolicy: "File",
		ImagePullPolicy:          k8scorev1.PullPolicy(imagePullPolicy),
	}

	// Avoid duplicate mountPaths when users provide filesFromConfigMap that collide with
	// clabernetes-managed mounts (for example, "topo.clab.yaml" which is always mounted by
	// clabernetes itself). Kubernetes rejects duplicate mountPaths.
	{
		seen := map[string]struct{}{}
		for i := range launcherContainer.VolumeMounts {
			seen[strings.TrimSpace(launcherContainer.VolumeMounts[i].MountPath)] = struct{}{}
		}
		for _, vm := range volumeMountsFromCommonSpec {
			mp := strings.TrimSpace(vm.MountPath)
			if mp == "" {
				continue
			}
			if _, ok := seen[mp]; ok {
				continue
			}
			launcherContainer.VolumeMounts = append(launcherContainer.VolumeMounts, vm)
			seen[mp] = struct{}{}
		}
	}

	if !nativeMode {
		deployment.Spec.Template.Spec.Containers = []k8scorev1.Container{launcherContainer}

		return
	}

	// native mode, so we have the sidecar and the nos container
	launcherContainer.Name = "clabernetes-launcher"

	nodeImage := clabernetesConfigs[nodeName].Topology.GetNodeImage(nodeName)

	nosContainer := k8scorev1.Container{
		Name:  nodeName,
		Image: nodeImage,
		VolumeMounts: []k8scorev1.VolumeMount{
			{
				Name:      "docker",
				ReadOnly:  false,
				MountPath: "/clabernetes",
			},
		},
		TerminationMessagePath:   "/dev/termination-log",
		TerminationMessagePolicy: "File",
		ImagePullPolicy:          k8scorev1.PullPolicy(imagePullPolicy),
	}

	// Best-effort support for common containerlab node overrides when running in native mode.
	// For linux-based nodes, `cmd:` in the topology is commonly used (for example `sleep infinity`).
	if nodeDef, ok := clabernetesConfigs[nodeName].Topology.Nodes[nodeName]; ok {
		if strings.EqualFold(nodeDef.Kind, "linux") {
			cmd := strings.TrimSpace(nodeDef.Cmd)
			if cmd == "" {
				lc := strings.ToLower(strings.TrimSpace(nodeImage))
				// Common netlab examples use python/alpine images for linux hosts. In Kubernetes,
				// these can exit immediately when STDIN is not attached. Force a long-running
				// command so the node stays up for exec/config steps.
				if strings.Contains(lc, "python") || strings.Contains(lc, "alpine") {
					cmd = "sleep infinity"
				}
			}
			if cmd != "" {
				nosContainer.Command = []string{"sh", "-c", cmd}
			}
		}

		if len(nodeDef.Env) > 0 {
			for k, v := range nodeDef.Env {
				nosContainer.Env = append(
					nosContainer.Env,
					k8scorev1.EnvVar{Name: k, Value: v},
				)
			}
			slices.SortFunc(nosContainer.Env, func(a, b k8scorev1.EnvVar) int { return strings.Compare(a.Name, b.Name) })
		}

		// systemd-based NOS images often require writable tmpfs mounts.
		// In containerlab/Docker mode these are commonly configured by containerlab runtime flags;
		// in native mode we must provide them as Kubernetes volumes.
		if strings.EqualFold(nodeDef.Kind, "ceos") || strings.EqualFold(nodeDef.Kind, "eos") {
			existingMounts := map[string]struct{}{}
			for _, vm := range nosContainer.VolumeMounts {
				existingMounts[strings.TrimSpace(vm.MountPath)] = struct{}{}
			}
			existingVolumes := map[string]struct{}{}
			for _, v := range deployment.Spec.Template.Spec.Volumes {
				existingVolumes[v.Name] = struct{}{}
			}

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
					deployment.Spec.Template.Spec.Volumes = append(
						deployment.Spec.Template.Spec.Volumes,
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
				nosContainer.VolumeMounts = append(
					nosContainer.VolumeMounts,
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

			// In containerlab, the ceos node driver:
			// - sets critical env vars (CEOS/EOS_PLATFORM/MAPETH0/MGMT_INTF/INTFTYPE/...)
			// - injects these via systemd.setenv at /sbin/init start
			// - places startup-config in /mnt/flash/startup-config
			//
			// In clabernetes native mode, we must replicate the minimum required pieces for ceos
			// to initialize (Sysdb/ConfigAgent) and bring up SSH on the management plane.

			// Ensure startup-config is mounted into the NOS container at the location expected by ceos.
			//
			// In clabernetes, startup-config is typically rendered into a ConfigMap that contains one key per
			// node (like "l1-startup.cfg") and that key is mounted at the filePath that containerlab expects
			// (nodeDef.StartupConfig). We identify the correct ConfigMap/key by matching FilePath.
			startupConfigPath := ""
			if strings.TrimSpace(nodeDef.StartupConfig) != "" {
				startupConfigPath = strings.TrimSpace(nodeDef.StartupConfig)
			}
			if startupConfigPath != "" {
				for _, f := range owningTopology.Spec.Deployment.FilesFromConfigMap[nodeName] {
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

					if _, ok := existingMounts["/mnt/flash/startup-config"]; ok {
						break
					}

					nosContainer.VolumeMounts = append(
						nosContainer.VolumeMounts,
						k8scorev1.VolumeMount{
							Name:      volumeName,
							ReadOnly:  true,
							MountPath: "/mnt/flash/startup-config",
							SubPath:   f.ConfigMapPath,
						},
					)
					existingMounts["/mnt/flash/startup-config"] = struct{}{}
					break
				}
			}

			// Ensure the ceos-required env vars exist and are passed via systemd.setenv.
			//
			// NOTE: INTFTYPE must match the actual pod interfaces created for data-plane links.
			// Netlab commonly uses `etX` endpoint names (l1:et1, l1:et2, ...) which we preserve,
			// so we must not unconditionally override INTFTYPE here.
			ceosEnv := map[string]string{
				"CEOS":                                "1",
				"EOS_PLATFORM":                        "ceoslab",
				"container":                           "docker",
				"ETBA":                                "1",
				"SKIP_ZEROTOUCH_BARRIER_IN_SYSDBINIT": "1",
				// containerlab defaults that help cEOS boot reliably
				"MAPETH0":   "1",
				"MGMT_INTF": "eth0",
			}

			existingEnv := map[string]string{}
			for _, e := range nosContainer.Env {
				existingEnv[strings.TrimSpace(e.Name)] = e.Value
			}

			// Preserve INTFTYPE from the node definition when present (e.g. "et" from netlab),
			// otherwise default to "eth".
			intfType := strings.TrimSpace(existingEnv["INTFTYPE"])
			if intfType == "" {
				intfType = "eth"
			}
			ceosEnv["INTFTYPE"] = intfType

			// In Skyforge, we expect management reachability to come from the Kubernetes pod network,
			// not a separate "management VRF" inside the NOS container. Passing CLAB_MGMT_VRF through
			// to cEOS can result in the pod network interface being repurposed and losing the pod IP.
			//
			// Remove it to preserve Kubernetes/Cilium networking for SSH reachability from the in-cluster collector.
			delete(existingEnv, "CLAB_MGMT_VRF")
			nosContainer.Env = slices.DeleteFunc(nosContainer.Env, func(ev k8scorev1.EnvVar) bool {
				return strings.TrimSpace(ev.Name) == "CLAB_MGMT_VRF"
			})

			upsertEnv := func(key, value string) {
				key = strings.TrimSpace(key)
				if key == "" {
					return
				}
				existingEnv[key] = value

				for i := range nosContainer.Env {
					if nosContainer.Env[i].Name == key {
						nosContainer.Env[i].Value = value
						return
					}
				}
				nosContainer.Env = append(nosContainer.Env, k8scorev1.EnvVar{Name: key, Value: value})
			}

			// Always enforce these values for ceos.
			for k, v := range ceosEnv {
				upsertEnv(k, v)
			}

			slices.SortFunc(nosContainer.Env, func(a, b k8scorev1.EnvVar) int { return strings.Compare(a.Name, b.Name) })

			// Start /sbin/init with systemd.setenv arguments so ceos init code sees these values early.
			//
			// We use bash -c 'exec ...' so PID 1 remains /sbin/init.
			var setenvKeys []string
			for k := range ceosEnv {
				setenvKeys = append(setenvKeys, k)
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
			nosContainer.Command = []string{"bash", "-c", strings.TrimSpace(cmd.String())}
		}

		// vrnetlab-based Cisco IOL requires containerlab kind-driver setup (NETMAP/iouyap.ini/nvram),
		// which is normally performed by containerlab during deploy. In clabernetes native mode we
		// run the NOS container directly, so we must reproduce those runtime artifacts.
		//
		// Note: IOL interfaces generated by netlab may not be valid Linux ifnames (e.g. "Ethernet0/1");
		// clabernetes vxlan connectivity sanitizes those into Linux-safe interface names. We map the
		// IOL IOUYAP ports to those sanitized ifnames.
		if strings.EqualFold(nodeDef.Kind, "cisco_iol") {
			existingMounts := map[string]struct{}{}
			for _, vm := range nosContainer.VolumeMounts {
				existingMounts[strings.TrimSpace(vm.MountPath)] = struct{}{}
			}
			existingVolumes := map[string]struct{}{}
			for _, v := range deployment.Spec.Template.Spec.Volumes {
				existingVolumes[v.Name] = struct{}{}
			}

			ensureEmptyDir := func(volName string) {
				volName = strings.TrimSpace(volName)
				if volName == "" {
					return
				}
				if _, ok := existingVolumes[volName]; ok {
					return
				}
				deployment.Spec.Template.Spec.Volumes = append(
					deployment.Spec.Template.Spec.Volumes,
					k8scorev1.Volume{
						Name: volName,
						VolumeSource: k8scorev1.VolumeSource{
							EmptyDir: &k8scorev1.EmptyDirVolumeSource{},
						},
					},
				)
				existingVolumes[volName] = struct{}{}
			}

			runtimeVol := "vrnetlab-runtime"
			ensureEmptyDir(runtimeVol)

			if _, ok := existingMounts["/vrnetlab"]; !ok {
				nosContainer.VolumeMounts = append(
					nosContainer.VolumeMounts,
					k8scorev1.VolumeMount{
						Name:      runtimeVol,
						ReadOnly:  false,
						MountPath: "/vrnetlab",
					},
				)
				existingMounts["/vrnetlab"] = struct{}{}
			}

			sum := sha1.Sum([]byte(strings.TrimSpace(owningTopology.Name) + ":" + strings.TrimSpace(nodeName))) //nolint:gosec // non-crypto identifier
			pid := int(binary.BigEndian.Uint16(sum[:2])%1023) + 1
			nvramName := fmt.Sprintf("nvram_%05d", pid)

			// Mount the netlab-generated initial config snippet so we can incorporate it into the
			// boot config we provide to the IOL process.
			if _, ok := existingMounts["/netlab/initial.cfg"]; !ok {
				for _, f := range owningTopology.Spec.Deployment.FilesFromConfigMap[nodeName] {
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
					nosContainer.VolumeMounts = append(
						nosContainer.VolumeMounts,
						k8scorev1.VolumeMount{
							Name:      volumeName,
							ReadOnly:  true,
							MountPath: "/netlab/initial.cfg",
							SubPath:   f.ConfigMapPath,
						},
					)
					existingMounts["/netlab/initial.cfg"] = struct{}{}
					break
				}
			}

			upsertEnv := func(key, value string) {
				key = strings.TrimSpace(key)
				if key == "" {
					return
				}
				for i := range nosContainer.Env {
					if strings.TrimSpace(nosContainer.Env[i].Name) == key {
						nosContainer.Env[i].Value = value
						return
					}
				}
				nosContainer.Env = append(nosContainer.Env, k8scorev1.EnvVar{Name: key, Value: value})
			}

			upsertEnv("IOL_PID", strconv.Itoa(pid))
			upsertEnv("SKYFORGE_IOL_NVRAM", nvramName)

			// Determine the set of link interface names for this node, based on the containerlab links.
			// These interfaces are created by the clabernetes launcher vxlan connectivity manager.
			linkIfacesSet := map[string]struct{}{}
			var linkIfaces []string
			if clabernetesConfigs != nil && clabernetesConfigs[nodeName] != nil {
				for _, l := range clabernetesConfigs[nodeName].Topology.Links {
					if len(l.Endpoints) != 2 {
						continue
					}
					for _, ep := range l.Endpoints {
						parts := strings.SplitN(strings.TrimSpace(ep), ":", 2)
						if len(parts) != 2 {
							continue
						}
						if parts[0] != nodeName {
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
			}
			slices.Sort(linkIfaces)

			upsertEnv("SKYFORGE_IOL_LINK_IFACES", strings.Join(linkIfaces, ","))

			// Override the container entrypoint so we can:
			// - avoid the vrnetlab entrypoint's `grep eth` assumptions (netlab uses names like Ethernet0/1)
			// - generate the containerlab runtime artifacts expected by the IOL process
			// - incorporate netlab config snippet while ensuring SSH is enabled
			//
			// This is intentionally "best effort" and does not attempt to replicate every containerlab
			// behavior, only the minimal required pieces to get IOL booted and reachable.
			nosContainer.Command = []string{"bash", "-lc", strings.TrimSpace(fmt.Sprintf(`
set -euo pipefail
echo "[skyforge] vrnetlab iol bootstrap starting (node=%s pid=%d)"
node="%s"

IFS=',' read -r -a link_ifaces <<< "${SKYFORGE_IOL_LINK_IFACES:-}"
for ifn in "${link_ifaces[@]}"; do
  if [ -z "$ifn" ]; then
    continue
  fi
  for i in $(seq 1 90); do
    if [ -e "/sys/class/net/$ifn" ]; then
      break
    fi
    sleep 1
  done
done

mkdir -p /vrnetlab
touch "/vrnetlab/${SKYFORGE_IOL_NVRAM}"

# NETMAP: map ios ports to linux ifaces configured in iouyap.ini.
{
  echo "${IOL_PID}:0/0 513:0/0"
  idx=1
  for ifn in "${link_ifaces[@]}"; do
    if [ -z "$ifn" ]; then
      continue
    fi
    slot=$((idx / 4))
    port=$((idx %% 4))
    echo "${IOL_PID}:${slot}/${port} 513:${slot}/${port}"
    idx=$((idx+1))
  done
} > /vrnetlab/NETMAP

# Build /iol/config.txt (IOS boot config) similar to containerlab's iol kind driver,
# but using the dedicated vrnetlab Multus management interface (net1).
#
# IMPORTANT: We *must* remove the assigned IP from net1 in the Linux network namespace,
# otherwise the kernel will consume TCP/22 for that address and IOS will never see SSH
# traffic. By moving IOS mgmt to net1 we preserve eth0 for Kubernetes control plane traffic.
for i in $(seq 1 30); do
  if [ -e "/sys/class/net/net1" ]; then
    break
  fi
  sleep 1
done
# net1 can show up slightly before its IPv4 address is assigned by CNI.
# Retry for a bit to avoid crashlooping on fast startups.
ip link set net1 up 2>/dev/null || true
mgmt_ip=""
mgmt_gw=""
for i in $(seq 1 60); do
  mgmt_ip="$(ip -4 -o addr show dev net1 2>/dev/null | awk '{print $4}' | cut -d/ -f1 || true)"
  mgmt_gw="$(ip route show dev net1 2>/dev/null | awk '/^default/ {print $3; exit}' || true)"
  if [ -n "${mgmt_ip}" ]; then
    break
  fi
  sleep 1
done

if [ -z "${mgmt_ip}" ] && [ -f /vrnetlab/mgmt_ip ]; then
  mgmt_ip="$(cat /vrnetlab/mgmt_ip 2>/dev/null || true)"
fi

if [ -z "${mgmt_ip}" ]; then
  echo "[skyforge] vrnetlab iol mgmt interface net1 missing or has no IPv4 address"
  exit 1
fi

echo "${mgmt_ip}" > /vrnetlab/mgmt_ip || true

# If net1 is a veth (Multus bridge CNI), we've observed that direct raw-socket access to
# net1 can be unreliable. In that case, bridge net1 to a veth pair and have IOUYAP use
# the veth peer. If net1 is ipvlan (our preferred mode), IOUYAP can use net1 directly.
MGMT_DEV="net1"
if ip -d link show net1 2>/dev/null | grep -q " veth "; then
  BR="br-iol-mgmt"
  VETH_HOST="veth-mgmt"
  VETH_IOU="veth-iou"
  ip link add "$BR" type bridge 2>/dev/null || true
  ip link set "$BR" up
  ip link set net1 master "$BR" 2>/dev/null || true
  ip link set net1 up
  ip link add "$VETH_HOST" type veth peer name "$VETH_IOU" 2>/dev/null || true
  ip link set "$VETH_HOST" up
  ip link set "$VETH_IOU" up
  ip link set "$VETH_HOST" master "$BR" 2>/dev/null || true
  MGMT_DEV="$VETH_IOU"
fi
# Always ensure the vrnetlab mgmt interface is up. If it's left DOWN, iouyap will
# exit with "Network is down", and IOS will never get a working management plane.
ip link set net1 up 2>/dev/null || true
ip addr flush dev net1 || true

# IOUYAP config mapping bay/unit ports to linux ifaces.
{
  echo "[default]"
  echo "base_port = 49000"
  echo "netmap = /iol/NETMAP"
  echo "[513:0/0]"
  # Management interface:
  # - net1 comes from the vrnetlab-mgmt Multus attachment.
  # - eth0 must remain reserved for Kubernetes/Cilium so the clabernetes launcher can reach
  #   the API server and manage the pod.
  echo "eth_dev = ${MGMT_DEV}"
  idx=1
  for ifn in "${link_ifaces[@]}"; do
    if [ -z "$ifn" ]; then
      continue
    fi
    slot=$((idx / 4))
    port=$((idx %% 4))
    echo "[513:${slot}/${port}]"
    echo "eth_dev = $ifn"
    idx=$((idx+1))
  done
} > /vrnetlab/iouyap.ini

: > /vrnetlab/config.txt
cat >> /vrnetlab/config.txt <<CFGEOF
hostname ${node}
!
no aaa new-model
!
ip domain name lab
!
ip cef
!
ipv6 unicast-routing
!
no ip domain lookup
!
username admin privilege 15 secret admin
!
vrf definition clab-mgmt
 description clab-mgmt
 address-family ipv4
 !
 address-family ipv6
 !
!
interface Ethernet0/0
 vrf forwarding clab-mgmt
 description clab-mgmt
 ip address ${mgmt_ip} 255.255.255.0
 no shutdown
!
ip forward-protocol nd
!
ip ssh version 2
crypto key generate rsa modulus 2048
!
line vty 0 4
 login local
 transport input ssh
!
CFGEOF

if [ -n "${mgmt_gw}" ]; then
  echo "ip route vrf clab-mgmt 0.0.0.0 0.0.0.0 Ethernet0/0 ${mgmt_gw}" >> /vrnetlab/config.txt
  echo "!" >> /vrnetlab/config.txt
fi

	if [ -f /netlab/initial.cfg ]; then
	  # netlab-generated initial.cfg may include its own "line vty" stanza. That can
	  # unintentionally disable SSH access. Strip it and re-assert SSH on the vty lines below.
	  #
	  # It also commonly includes an "interface Ethernet0/0" stanza (management interface for IOL)
	  # which can override the vrf/ip config we generate above. Strip that too.
	  # NOTE: we intentionally avoid sed here. BusyBox sed can fail to parse the
	  # "Ethernet0/0" address range expression, causing the container to crashloop.
		  awk '
		    BEGIN { in_vty=0; in_mgmt_if=0 }
		    $0 == "line vty 0 4" { in_vty=1; next }
		    $0 == "interface Ethernet0/0" { in_mgmt_if=1; next }
		    in_vty {
		      if ($0 == "!") { in_vty=0 }
		      next
		    }
	    in_mgmt_if {
	      if ($0 == "!") { in_mgmt_if=0 }
	      next
	    }
	    { print }
	  ' /netlab/initial.cfg >> /vrnetlab/config.txt
	  echo "!" >> /vrnetlab/config.txt
	fi

cat >> /vrnetlab/config.txt <<CFGEOF
line vty 0 4
 login local
 transport input ssh
!
CFGEOF

echo "end" >> /vrnetlab/config.txt

# Symlink the runtime artifacts into /iol to match containerlab expectations.
ln -sf /vrnetlab/NETMAP /iol/NETMAP
ln -sf /vrnetlab/iouyap.ini /iol/iouyap.ini
ln -sf /vrnetlab/config.txt /iol/config.txt
ln -sf "/vrnetlab/${SKYFORGE_IOL_NVRAM}" "/iol/${SKYFORGE_IOL_NVRAM}"

# Start iouyap (background) + IOL.
/usr/bin/iouyap -f /iol/iouyap.ini 513 -q -d

ports=$(( ${#link_ifaces[@]} + 1 ))
slots=$(( (ports + 3) / 4 ))
echo "[skyforge] starting iol.bin (slots=$slots ports=$ports mgmt_ip=${mgmt_ip} gw=${mgmt_gw})"
cd /iol
exec ./iol.bin "$IOL_PID" -e "$slots" -s 0 -c config.txt -n 1024
`, nodeName, pid, nodeName))}
		}

		// Best-effort support for bind mounts in native mode.
		//
		// In "classic" (non-native) mode, containerlab/Docker handles common node requirements for
		// network OSes. In native mode, the NOS runs as a Kubernetes container and relies on k8s
		// volume mounts. We translate *absolute* host bind mounts into HostPath volumes.
		if len(nodeDef.Binds) > 0 {
			existingMounts := map[string]struct{}{}
			for _, vm := range nosContainer.VolumeMounts {
				existingMounts[strings.TrimSpace(vm.MountPath)] = struct{}{}
			}
			existingVolumes := map[string]struct{}{}
			for _, v := range deployment.Spec.Template.Spec.Volumes {
				existingVolumes[v.Name] = struct{}{}
			}

			for idx, bind := range nodeDef.Binds {
				bind = strings.TrimSpace(bind)
				if bind == "" {
					continue
				}
				parts := strings.SplitN(bind, ":", 3)
				if len(parts) < 2 {
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
				if len(parts) == 3 {
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

				deployment.Spec.Template.Spec.Volumes = append(
					deployment.Spec.Template.Spec.Volumes,
					k8scorev1.Volume{
						Name: volName,
						VolumeSource: k8scorev1.VolumeSource{
							HostPath: &k8scorev1.HostPathVolumeSource{
								Path: hostPath,
							},
						},
					},
				)
				nosContainer.VolumeMounts = append(
					nosContainer.VolumeMounts,
					k8scorev1.VolumeMount{
						Name:      volName,
						MountPath: containerPath,
						ReadOnly:  readOnly,
					},
				)
			}
		}

		if strings.TrimSpace(nodeDef.Cmd) != "" {
			nosContainer.Command = []string{"sh", "-c", nodeDef.Cmd}
		} else if strings.TrimSpace(nodeDef.Entrypoint) != "" {
			nosContainer.Command = []string{"sh", "-c", nodeDef.Entrypoint}
		}
	}

	deployment.Spec.Template.Spec.Containers = []k8scorev1.Container{nosContainer, launcherContainer}
}

func (r *DeploymentReconciler) renderDeploymentMultus(
	deployment *k8sappsv1.Deployment,
	owningTopology *clabernetesapisv1alpha1.Topology,
	nodeName string,
	clabernetesConfigs map[string]*clabernetesutilcontainerlab.Config,
) {
	if owningTopology.Spec.Connectivity != clabernetesconstants.ConnectivityMultus {
		return
	}

	r.log.Debugf("multus connectivity enabled for topology %s", owningTopology.Name)

	nodeConfig, ok := clabernetesConfigs[nodeName]
	if !ok {
		return
	}

	// Keep NAD naming consistent with NetworkAttachmentDefinitionReconciler.Resolve,
	// which uses nodeConfig.Name as the link namespace prefix.
	topologyName := nodeConfig.Name

	var networkNames []string

	for idx := range nodeConfig.Topology.Links {
		// we use the index from the original links slice as the unique ID for the NAD
		// this assumes the link order is stable, which it should be since it comes from
		// the same topology object.
		nadName := fmt.Sprintf("%s-l%d", topologyName, idx)
		networkNames = append(networkNames, nadName)
	}

	if len(networkNames) == 0 {
		return
	}

	// annotation format: k8s.v1.cni.cncf.io/networks: '[{"name": "nad1"}, {"name": "nad2"}]'
	// for simplicity we'll just use the short format if possible, but let's do the json one
	// to be explicit and future-proof.
	type multusNet struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace,omitempty"`
	}

	multusNets := make([]multusNet, len(networkNames))
	for i, name := range networkNames {
		multusNets[i] = multusNet{Name: name}
	}

	// If this node runs a vrnetlab-based NOS (IOL/VIOS/NXOSv/etc), add a dedicated
	// management network attachment.
	//
	// Many vrnetlab images assume they can take over eth0 and may flush its IP
	// addresses. In Kubernetes, eth0 is the pod network, so losing it breaks
	// pod connectivity. A secondary Multus interface gives the NOS an interface
	// it can own without affecting the pod network.
	//
	// This NetworkAttachmentDefinition is installed by Skyforge Helm as:
	//   kube-system/vrnetlab-mgmt
	if node, ok := nodeConfig.Topology.Nodes[nodeName]; ok {
		switch strings.TrimSpace(node.Kind) {
		case "cisco_iol", "vios", "viosl2", "vr-n9kv", "asav", "vmx", "sros", "csr":
			multusNets = append(multusNets, multusNet{Name: "vrnetlab-mgmt", Namespace: "kube-system"})
		}
	}

	multusNetsJSON, err := json.Marshal(multusNets)
	if err != nil {
		r.log.Criticalf("failed marshaling multus networks to json, error: %s", err)

		return
	}

	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}

	deployment.Spec.Template.Annotations["k8s.v1.cni.cncf.io/networks"] = string(multusNetsJSON)
}

func (r *DeploymentReconciler) renderDeploymentVrnetlabMgmt(
	deployment *k8sappsv1.Deployment,
	nodeName string,
	clabernetesConfigs map[string]*clabernetesutilcontainerlab.Config,
) {
	nodeConfig, ok := clabernetesConfigs[nodeName]
	if !ok {
		return
	}
	node, ok := nodeConfig.Topology.Nodes[nodeName]
	if !ok {
		return
	}

	switch strings.TrimSpace(node.Kind) {
	case "cisco_iol", "vios", "viosl2", "vr-n9kv", "asav", "vmx", "sros", "csr":
	default:
		return
	}

	type multusNet struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace,omitempty"`
	}

	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}

	cur := strings.TrimSpace(deployment.Spec.Template.Annotations["k8s.v1.cni.cncf.io/networks"])
	nets := []multusNet{}
	if cur != "" {
		_ = json.Unmarshal([]byte(cur), &nets)
	}
	for _, n := range nets {
		if strings.TrimSpace(n.Name) == "vrnetlab-mgmt" && strings.TrimSpace(n.Namespace) == "kube-system" {
			return
		}
	}
	nets = append(nets, multusNet{Name: "vrnetlab-mgmt", Namespace: "kube-system"})

	raw, err := json.Marshal(nets)
	if err != nil {
		r.log.Criticalf("failed marshaling vrnetlab mgmt multus networks to json, error: %s", err)
		return
	}
	deployment.Spec.Template.Annotations["k8s.v1.cni.cncf.io/networks"] = string(raw)
}

func (r *DeploymentReconciler) renderDeploymentNative(
	deployment *k8sappsv1.Deployment,
	nodeName,
	configVolumeName string,
	volumeMountsFromCommonSpec []k8scorev1.VolumeMount,
	owningTopology *clabernetesapisv1alpha1.Topology,
	clabernetesConfigs map[string]*clabernetesutilcontainerlab.Config,
) {
	if !ResolveNativeMode(owningTopology) {
		return
	}

	// When ShareProcessNamespace is enabled, the "native" NOS container may not end up as PID 1.
	// Systemd-based NOS images (like Arista cEOS) require being PID 1 and will crash otherwise.
	//
	// Disable process namespace sharing for these nodes while still allowing native mode.
	if clabernetesConfigs != nil && clabernetesConfigs[nodeName] != nil {
		if node, ok := clabernetesConfigs[nodeName].Topology.Nodes[nodeName]; ok {
			switch node.Kind {
			case "ceos", "eos":
				deployment.Spec.Template.Spec.ShareProcessNamespace = nil
			}
		}
	}

	launcherContainer := r.getLauncherContainer(deployment)

	initContainer := launcherContainer.DeepCopy()
	initContainer.Name = "clabernetes-setup"
	initContainer.Command = []string{"/clabernetes/manager", "setup"}

	deployment.Spec.Template.Spec.InitContainers = append(
		deployment.Spec.Template.Spec.InitContainers,
		*initContainer,
	)
}

func (r *DeploymentReconciler) getLauncherContainer(
	deployment *k8sappsv1.Deployment,
) *k8scorev1.Container {
	for i := range deployment.Spec.Template.Spec.Containers {
		if deployment.Spec.Template.Spec.Containers[i].Name == "clabernetes-launcher" {
			return &deployment.Spec.Template.Spec.Containers[i]
		}
	}

	return &deployment.Spec.Template.Spec.Containers[0]
}

func (r *DeploymentReconciler) renderDeploymentContainerEnv( //nolint: funlen
	deployment *k8sappsv1.Deployment,
	nodeName,
	owningTopologyName string,
	owningTopology *clabernetesapisv1alpha1.Topology,
	clabernetesConfigs map[string]*clabernetesutilcontainerlab.Config,
) {
	launcherLogLevel := owningTopology.Spec.Deployment.LauncherLogLevel
	if launcherLogLevel == "" {
		launcherLogLevel = r.configManagerGetter().GetLauncherLogLevel()
	}

	imagePullThroughMode := owningTopology.Spec.ImagePull.PullThroughOverride
	if owningTopology.Spec.ImagePull.PullThroughOverride == "" {
		imagePullThroughMode = r.configManagerGetter().GetImagePullThroughMode()
	}

	criKind := r.configManagerGetter().GetImagePullCriKindOverride()
	if criKind == "" {
		criKind = r.criKind
	}

	nodeImage := clabernetesConfigs[nodeName].Topology.GetNodeImage(nodeName)
	if nodeImage == "" {
		r.log.Warnf(
			"could not parse image for node %q, topology in question printined in debug log",
			nodeName,
		)

		subTopologyBytes, err := json.MarshalIndent(clabernetesConfigs[nodeName], "", "    ")
		if err != nil {
			r.log.Warnf("failed marshaling topology, error: %s", err)
		} else {
			r.log.Debugf("node topology:\n%s", string(subTopologyBytes))
		}
	}

	containerlabVersion := owningTopology.Spec.Deployment.ContainerlabVersion
	if containerlabVersion == "" {
		containerlabVersion = r.configManagerGetter().GetContainerlabVersion()
	}

	containerlabTimeout := owningTopology.Spec.Deployment.ContainerlabTimeout
	if containerlabTimeout == "" {
		containerlabTimeout = r.configManagerGetter().GetContainerlabTimeout()
	}

	envs := []k8scorev1.EnvVar{
		{
			Name: clabernetesconstants.NodeNameEnv,
			ValueFrom: &k8scorev1.EnvVarSource{
				FieldRef: &k8scorev1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "spec.nodeName",
				},
			},
		},
		{
			Name: clabernetesconstants.PodNameEnv,
			ValueFrom: &k8scorev1.EnvVarSource{
				FieldRef: &k8scorev1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
			},
		},
		{
			Name: clabernetesconstants.PodNamespaceEnv,
			ValueFrom: &k8scorev1.EnvVarSource{
				FieldRef: &k8scorev1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.namespace",
				},
			},
		},
		{
			Name:  clabernetesconstants.AppNameEnv,
			Value: r.managerAppName,
		},
		{
			Name:  clabernetesconstants.ManagerNamespaceEnv,
			Value: r.managerNamespace,
		},
		{
			Name:  clabernetesconstants.LauncherCRIKindEnv,
			Value: criKind,
		},
		{
			Name:  clabernetesconstants.LauncherImagePullThroughModeEnv,
			Value: imagePullThroughMode,
		},
		{
			Name:  clabernetesconstants.LauncherLoggerLevelEnv,
			Value: launcherLogLevel,
		},
		{
			Name:  clabernetesconstants.LauncherTopologyNameEnv,
			Value: owningTopologyName,
		},
		{
			Name:  clabernetesconstants.LauncherNodeNameEnv,
			Value: nodeName,
		},
		{
			Name:  clabernetesconstants.LauncherNodeImageEnv,
			Value: nodeImage,
		},
		{
			Name:  clabernetesconstants.LauncherConnectivityKind,
			Value: owningTopology.Spec.Connectivity,
		},
		{
			Name:  clabernetesconstants.LauncherContainerlabVersion,
			Value: containerlabVersion,
		},
		{
			Name:  clabernetesconstants.LauncherContainerlabTimeout,
			Value: containerlabTimeout,
		},
	}

	if ResolveNativeMode(owningTopology) {
		envs = append(
			envs,
			k8scorev1.EnvVar{
				Name:  clabernetesconstants.LauncherNativeModeEnv,
				Value: clabernetesconstants.True,
			},
		)
	}

	if ResolveGlobalVsTopologyBool(
		r.configManagerGetter().GetContainerlabDebug(),
		owningTopology.Spec.Deployment.ContainerlabDebug,
	) {
		envs = append(
			envs,
			k8scorev1.EnvVar{
				Name:  clabernetesconstants.LauncherContainerlabDebug,
				Value: clabernetesconstants.True,
			},
		)
	}

	if owningTopology.Spec.Deployment.Persistence.Enabled {
		envs = append(
			envs,
			k8scorev1.EnvVar{
				Name:  clabernetesconstants.LauncherContainerlabPersist,
				Value: clabernetesconstants.True,
			},
		)
	}

	if len(owningTopology.Spec.ImagePull.InsecureRegistries) > 0 {
		envs = append(
			envs,
			k8scorev1.EnvVar{
				Name:  clabernetesconstants.LauncherInsecureRegistries,
				Value: strings.Join(owningTopology.Spec.ImagePull.InsecureRegistries, ","),
			},
		)
	}

	if ResolveGlobalVsTopologyBool(
		r.configManagerGetter().GetPrivilegedLauncher(),
		owningTopology.Spec.Deployment.PrivilegedLauncher,
	) {
		envs = append(
			envs,
			k8scorev1.EnvVar{
				Name:  clabernetesconstants.LauncherPrivilegedEnv,
				Value: clabernetesconstants.True,
			},
		)
	}

	if len(owningTopology.Spec.Deployment.ExtraEnv) > 0 {
		envs = append(
			envs,
			owningTopology.Spec.Deployment.ExtraEnv...,
		)
	} else {
		globalEnvs := r.configManagerGetter().GetExtraEnv()

		envs = append(
			envs,
			globalEnvs...,
		)
	}

	r.getLauncherContainer(deployment).Env = envs
	for i := range deployment.Spec.Template.Spec.InitContainers {
		if deployment.Spec.Template.Spec.InitContainers[i].Name == "clabernetes-setup" {
			deployment.Spec.Template.Spec.InitContainers[i].Env = envs
		}
	}
}

func (r *DeploymentReconciler) renderDeploymentContainerResources(
	deployment *k8sappsv1.Deployment,
	nodeName string,
	owningTopology *clabernetesapisv1alpha1.Topology,
	clabernetesConfigs map[string]*clabernetesutilcontainerlab.Config,
) {
	nodeResources, nodeResourcesOk := owningTopology.Spec.Deployment.Resources[nodeName]
	if nodeResourcesOk {
		r.getLauncherContainer(deployment).Resources = nodeResources

		return
	}

	defaultResources, defaultResourcesOk := owningTopology.Spec.Deployment.Resources[clabernetesconstants.Default] //nolint:lll
	if defaultResourcesOk {
		r.getLauncherContainer(deployment).Resources = defaultResources

		return
	}

	resources := r.configManagerGetter().GetResourcesForContainerlabKind(
		clabernetesConfigs[nodeName].Topology.GetNodeKindType(nodeName),
	)

	if resources != nil {
		r.getLauncherContainer(deployment).Resources = *resources
	}
}

func (r *DeploymentReconciler) renderDeploymentNodeSelectors(
	deployment *k8sappsv1.Deployment,
	nodeName string,
	owningTopology *clabernetesapisv1alpha1.Topology,
	clabernetesConfigs map[string]*clabernetesutilcontainerlab.Config,
) {
	nodeImage := clabernetesConfigs[nodeName].Topology.GetNodeImage(nodeName)

	nodeSelectors := r.configManagerGetter().GetNodeSelectorsByImage(nodeImage)
	if len(nodeSelectors) == 0 {
		maps.Copy(nodeSelectors, owningTopology.Spec.Deployment.Scheduling.NodeSelector)
	}

	deployment.Spec.Template.Spec.NodeSelector = nodeSelectors
}

func (r *DeploymentReconciler) renderDeploymentContainerPrivileges(
	deployment *k8sappsv1.Deployment,
	nodeName string,
	owningTopology *clabernetesapisv1alpha1.Topology,
) {
	if ResolveGlobalVsTopologyBool(
		r.configManagerGetter().GetPrivilegedLauncher(),
		owningTopology.Spec.Deployment.PrivilegedLauncher,
	) {
		for i := range deployment.Spec.Template.Spec.Containers {
			deployment.Spec.Template.Spec.Containers[i].SecurityContext = &k8scorev1.SecurityContext{
				Privileged: clabernetesutil.ToPointer(true),
				RunAsUser:  clabernetesutil.ToPointer(int64(0)),
			}
		}

		return
	}

	// w/out this set you cant remount /sys/fs/cgroup, /proc, and /proc/sys; note that the part
	// after the "/" needs to be the name of the container this applies to -- in our case (for now?)
	// this will always just be the node name
	deployment.ObjectMeta.Annotations[fmt.Sprintf(
		"%s/%s", "container.apparmor.security.beta.kubernetes.io", nodeName,
	)] = "unconfined"

	// if native mode is enabled, we also need to set the sidecar as unconfined
	if ResolveNativeMode(owningTopology) {
		deployment.ObjectMeta.Annotations[fmt.Sprintf(
			"%s/%s", "container.apparmor.security.beta.kubernetes.io", "clabernetes-launcher",
		)] = "unconfined"
	}

	for i := range deployment.Spec.Template.Spec.Containers {
		deployment.Spec.Template.Spec.Containers[i].SecurityContext = &k8scorev1.SecurityContext{
			Privileged: clabernetesutil.ToPointer(false),
			RunAsUser:  clabernetesutil.ToPointer(int64(0)),
			Capabilities: &k8scorev1.Capabilities{
				Add: []k8scorev1.Capability{
					// docker says we need these ones:
					// https://github.com/moby/moby/blob/master/oci/caps/defaults.go#L6-L19
					"CHOWN",
					"DAC_OVERRIDE",
					"FSETID",
					"FOWNER",
					"MKNOD",
					"NET_RAW",
					"SETGID",
					"SETUID",
					"SETFCAP",
					"SETPCAP",
					"NET_BIND_SERVICE",
					"SYS_CHROOT",
					"KILL",
					"AUDIT_WRITE",
					// docker doesnt say we need this but surely we do otherwise cant connect to
					// daemon
					"NET_ADMIN",
					// cant untar/load image w/out this it seems
					// https://github.com/moby/moby/issues/43086
					"SYS_ADMIN",
					// this it seems we need otherwise we get some issues finding child pid of
					// containers and when we "docker run" it craps out
					"SYS_RESOURCE",
					// and some more that we needed to boot srl
					"LINUX_IMMUTABLE",
					"SYS_BOOT",
					"SYS_TIME",
					"SYS_MODULE",
					"SYS_RAWIO",
					"SYS_PTRACE",
					// and some more that we need to run xdp lc manager in srl, and probably others!?
					"SYS_NICE",
					"IPC_LOCK",
				},
			},
		}
	}
}

func (r *DeploymentReconciler) renderDeploymentContainerStatus(
	deployment *k8sappsv1.Deployment,
	nodeName string,
	owningTopology *clabernetesapisv1alpha1.Topology,
) {
	if !owningTopology.Spec.StatusProbes.Enabled {
		return
	}

	if slices.Contains(owningTopology.Spec.StatusProbes.ExcludedNodes, nodeName) {
		// this clab node was excluded, dont setup probes
		return
	}

	nodeProbeConfiguration, ok := owningTopology.Spec.StatusProbes.NodeProbeConfigurations[nodeName]
	if !ok {
		nodeProbeConfiguration = owningTopology.Spec.StatusProbes.ProbeConfiguration
	}

	if nodeProbeConfiguration.SSHProbeConfiguration == nil &&
		nodeProbeConfiguration.TCPProbeConfiguration == nil {
		r.log.Warnf("node %q has no status probe configurations, skipping...", nodeName)

		return
	}

	// default failure threshold for startup probe == 40, 40*20 = 800 seconds startup probe total
	// time (plus the 60s initial delay) for 15ish min startup time...
	failureThresholds := probeDefaultStartupFailureThreshold

	if nodeProbeConfiguration.StartupSeconds != 0 {
		failureThresholds = nodeProbeConfiguration.StartupSeconds / probePeriodSeconds
	}

	// startup probe delays the start of the readiness probe -- this gives us time for the nos to
	// boot before we start doing the readiness check on the (slightly) faster frequency
	r.getLauncherContainer(deployment).StartupProbe = &k8scorev1.Probe{
		ProbeHandler: k8scorev1.ProbeHandler{
			Exec: &k8scorev1.ExecAction{
				Command: []string{
					"grep",
					clabernetesconstants.NodeStatusHealthy,
					clabernetesconstants.NodeStatusFile,
				},
			},
		},
		InitialDelaySeconds: probeInitialDelay,
		TimeoutSeconds:      1,
		SuccessThreshold:    1,
		PeriodSeconds:       probePeriodSeconds,
		FailureThreshold:    int32(failureThresholds), //nolint:gosec
	}

	// after the startup probe has done its thing we set run the readiness probe -- since the
	// launcher doenst check the status super frequently we keep this pretty slow too
	r.getLauncherContainer(deployment).ReadinessProbe = &k8scorev1.Probe{
		ProbeHandler: k8scorev1.ProbeHandler{
			Exec: &k8scorev1.ExecAction{
				Command: []string{
					"grep",
					clabernetesconstants.NodeStatusHealthy,
					clabernetesconstants.NodeStatusFile,
				},
			},
		},
		TimeoutSeconds:   1,
		SuccessThreshold: 1,
		PeriodSeconds:    probePeriodSeconds,
		FailureThreshold: probeReadinessFailureThreshold,
	}

	probeEnvVars := make([]k8scorev1.EnvVar, 0)

	if nodeProbeConfiguration.TCPProbeConfiguration != nil {
		probeEnvVars = append(
			probeEnvVars,
			k8scorev1.EnvVar{
				Name:  clabernetesconstants.LauncherTCPProbePort,
				Value: strconv.Itoa(nodeProbeConfiguration.TCPProbeConfiguration.Port),
			},
		)
	}

	if nodeProbeConfiguration.SSHProbeConfiguration != nil {
		probeEnvVars = append(
			probeEnvVars,
			k8scorev1.EnvVar{
				Name:  clabernetesconstants.LauncherSSHProbeUsername,
				Value: nodeProbeConfiguration.SSHProbeConfiguration.Username,
			},
			k8scorev1.EnvVar{
				Name:  clabernetesconstants.LauncherSSHProbePassword,
				Value: nodeProbeConfiguration.SSHProbeConfiguration.Password,
			},
		)

		if nodeProbeConfiguration.SSHProbeConfiguration.Port != 0 {
			probeEnvVars = append(
				probeEnvVars,
				k8scorev1.EnvVar{
					Name:  clabernetesconstants.LauncherSSHProbePort,
					Value: strconv.Itoa(nodeProbeConfiguration.SSHProbeConfiguration.Port),
				},
			)
		}
	}

	r.getLauncherContainer(deployment).Env = append(
		r.getLauncherContainer(deployment).Env,
		probeEnvVars...,
	)
}

func (r *DeploymentReconciler) renderDeploymentDevices(
	deployment *k8sappsv1.Deployment,
	owningTopology *clabernetesapisv1alpha1.Topology,
) {
	// Even in privileged mode, device nodes like /dev/kvm are not guaranteed to
	// exist in the container filesystem unless explicitly mounted. KVM-backed NOS
	// images (IOL/VIOS/vrnetlab/etc.) require /dev/kvm, and network OSes often need
	// /dev/net/tun. Provide these device mounts consistently for all containers.

	ensureVolume := func(name, hostPath string) {
		for _, v := range deployment.Spec.Template.Spec.Volumes {
			if v.Name == name {
				return
			}
		}
		deployment.Spec.Template.Spec.Volumes = append(
			deployment.Spec.Template.Spec.Volumes,
			k8scorev1.Volume{
				Name: name,
				VolumeSource: k8scorev1.VolumeSource{
					HostPath: &k8scorev1.HostPathVolumeSource{
						Path: hostPath,
						Type: clabernetesutil.ToPointer(k8scorev1.HostPathType("")),
					},
				},
			},
		)
	}

	ensureVolume("dev-kvm", "/dev/kvm")
	ensureVolume("dev-fuse", "/dev/fuse")
	ensureVolume("dev-net-tun", "/dev/net/tun")

	ensureMount := func(c *k8scorev1.Container, name, mountPath string) {
		if c == nil {
			return
		}
		for _, vm := range c.VolumeMounts {
			if vm.Name == name || strings.TrimSpace(vm.MountPath) == mountPath {
				return
			}
		}
		c.VolumeMounts = append(
			c.VolumeMounts,
			k8scorev1.VolumeMount{
				Name:      name,
				ReadOnly:  false,
				MountPath: mountPath,
			},
		)
	}

	for i := range deployment.Spec.Template.Spec.Containers {
		ensureMount(&deployment.Spec.Template.Spec.Containers[i], "dev-kvm", "/dev/kvm")
		ensureMount(&deployment.Spec.Template.Spec.Containers[i], "dev-fuse", "/dev/fuse")
		ensureMount(&deployment.Spec.Template.Spec.Containers[i], "dev-net-tun", "/dev/net/tun")
	}
}

func (r *DeploymentReconciler) renderDeploymentPersistence(
	deployment *k8sappsv1.Deployment,
	nodeName,
	owningTopologyName string,
	owningTopology *clabernetesapisv1alpha1.Topology,
) {
	if !owningTopology.Spec.Deployment.Persistence.Enabled {
		return
	}

	volumeName := "containerlab-directory-persistence"

	deployment.Spec.Template.Spec.Volumes = append(
		deployment.Spec.Template.Spec.Volumes,
		k8scorev1.Volume{
			Name: volumeName,
			VolumeSource: k8scorev1.VolumeSource{
				PersistentVolumeClaim: &k8scorev1.PersistentVolumeClaimVolumeSource{
					ClaimName: fmt.Sprintf("%s-%s", owningTopologyName, nodeName),
					ReadOnly:  false,
				},
			},
		},
	)

	r.getLauncherContainer(deployment).VolumeMounts = append(
		r.getLauncherContainer(deployment).VolumeMounts,
		k8scorev1.VolumeMount{
			Name:      volumeName,
			ReadOnly:  false,
			MountPath: fmt.Sprintf("/clabernetes/clab-clabernetes-%s", nodeName),
		},
	)
}

func determineNodeNeedsRestart(
	reconcileData *ReconcileData,
	nodeName string,
) {
	previousConfig := reconcileData.PreviousConfigs[nodeName]
	currentConfig := reconcileData.ResolvedConfigs[nodeName]

	// We store the "previous" configs in Topology status as YAML strings and
	// unmarshal them on each reconcile. Depending on which keys were present in
	// the YAML, slices/maps can legitimately round-trip as nil vs empty.
	//
	// Treat nil/empty collections as equivalent so we don't get stuck in a
	// perpetual restart loop.
	normalizeNilCollections(previousConfig)
	normalizeNilCollections(currentConfig)

	if previousConfig.Debug != currentConfig.Debug {
		reconcileData.NodesNeedingReboot.Add(nodeName)

		return
	}

	if previousConfig.Name != currentConfig.Name {
		reconcileData.NodesNeedingReboot.Add(nodeName)

		return
	}

	if !reflect.DeepEqual(previousConfig.Mgmt, currentConfig.Mgmt) {
		reconcileData.NodesNeedingReboot.Add(nodeName)

		return
	}

	if !reflect.DeepEqual(previousConfig.Prefix, currentConfig.Prefix) {
		reconcileData.NodesNeedingReboot.Add(nodeName)

		return
	}

	if !reflect.DeepEqual(previousConfig.Topology.Nodes, currentConfig.Topology.Nodes) {
		reconcileData.NodesNeedingReboot.Add(nodeName)

		return
	}

	if !reflect.DeepEqual(previousConfig.Topology.Kinds, currentConfig.Topology.Kinds) {
		reconcileData.NodesNeedingReboot.Add(nodeName)

		return
	}

	if !reflect.DeepEqual(previousConfig.Topology.Defaults, currentConfig.Topology.Defaults) {
		reconcileData.NodesNeedingReboot.Add(nodeName)

		return
	}

	if len(previousConfig.Topology.Links) != len(currentConfig.Topology.Links) {
		// dont bother checking links since they cant be same/same, node needs rebooted to restart
		// clab bits
		reconcileData.NodesNeedingReboot.Add(nodeName)

		return
	}

	// we know (because we set this) that topology will never be nil and links will always be slices
	// that are len 2... so we are a little risky here but its probably ok :)
	for idx := range previousConfig.Topology.Links {
		previousASide := previousConfig.Topology.Links[idx].Endpoints[0]
		currentASide := currentConfig.Topology.Links[idx].Endpoints[0]

		if previousASide == currentASide {
			// as long as "a" side is the same, things will auto update itself since launcher is
			// watching the connectivity cr
			continue
		}

		reconcileData.NodesNeedingReboot.Add(nodeName)

		return
	}
}

func normalizeNilCollections(obj any) {
	if obj == nil {
		return
	}

	val := reflect.ValueOf(obj)
	if val.Kind() != reflect.Pointer || val.IsNil() {
		return
	}

	normalizeNilCollectionsValue(val.Elem())
}

func normalizeNilCollectionsValue(val reflect.Value) {
	if !val.IsValid() {
		return
	}

	switch val.Kind() {
	case reflect.Pointer:
		if val.IsNil() {
			return
		}
		normalizeNilCollectionsValue(val.Elem())
	case reflect.Interface:
		if val.IsNil() {
			return
		}
		normalizeNilCollectionsValue(val.Elem())
	case reflect.Struct:
		for i := 0; i < val.NumField(); i++ {
			field := val.Field(i)

			switch field.Kind() {
			case reflect.Slice:
				if field.IsNil() && field.CanSet() {
					field.Set(reflect.MakeSlice(field.Type(), 0, 0))
				}
			case reflect.Map:
				if field.IsNil() && field.CanSet() {
					field.Set(reflect.MakeMap(field.Type()))
				}
			default:
				normalizeNilCollectionsValue(field)
			}
		}
	case reflect.Slice:
		for i := 0; i < val.Len(); i++ {
			normalizeNilCollectionsValue(val.Index(i))
		}
	case reflect.Map:
		for _, key := range val.MapKeys() {
			normalizeNilCollectionsValue(val.MapIndex(key))
		}
	}
}
