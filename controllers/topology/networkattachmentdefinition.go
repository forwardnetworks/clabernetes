package topology

import (
	"encoding/json"
	"fmt"
	"reflect"

	clabernetesapisv1alpha1 "github.com/srl-labs/clabernetes/apis/v1alpha1"
	clabernetesconfig "github.com/srl-labs/clabernetes/config"
	clabernetesconstants "github.com/srl-labs/clabernetes/constants"
	claberneteslogging "github.com/srl-labs/clabernetes/logging"
	clabernetesutil "github.com/srl-labs/clabernetes/util"
	clabernetesutilcontainerlab "github.com/srl-labs/clabernetes/util/containerlab"
	clabernetesutilkubernetes "github.com/srl-labs/clabernetes/util/kubernetes"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apimachinerytypes "k8s.io/apimachinery/pkg/types"
)

// NetworkAttachmentDefinitionReconciler is a subcomponent of the "TopologyReconciler" but is
// exposed for testing purposes. This is the component responsible for rendering/validating the
// NetworkAttachmentDefinition crs for the Topology.
type NetworkAttachmentDefinitionReconciler struct {
	log                 claberneteslogging.Instance
	configManagerGetter clabernetesconfig.ManagerGetterFunc
}

// NewNetworkAttachmentDefinitionReconciler returns an instance of
// NetworkAttachmentDefinitionReconciler.
func NewNetworkAttachmentDefinitionReconciler(
	log claberneteslogging.Instance,
	configManagerGetter clabernetesconfig.ManagerGetterFunc,
) *NetworkAttachmentDefinitionReconciler {
	return &NetworkAttachmentDefinitionReconciler{
		log:                 log,
		configManagerGetter: configManagerGetter,
	}
}

// Resolve accepts a mapping of clabernetes sub-topology configs and a list of NADs that are -- by
// owner reference and/or labels -- associated with the topology. It returns a ObjectDiffer object
// that contains the missing, extra, and current NADs for the topology.
func (r *NetworkAttachmentDefinitionReconciler) Resolve(
	ownedNADs *unstructured.UnstructuredList,
	clabernetesConfigs map[string]*clabernetesutilcontainerlab.Config,
	_ *clabernetesapisv1alpha1.Topology,
) (*clabernetesutil.ObjectDiffer[*unstructured.Unstructured], error) {
	nadDiffer := &clabernetesutil.ObjectDiffer[*unstructured.Unstructured]{
		Current: map[string]*unstructured.Unstructured{},
	}

	for i := range ownedNADs.Items {
		nadDiffer.Current[ownedNADs.Items[i].GetName()] = &ownedNADs.Items[i]
	}

	// link names are topologyName-l<index>
	// we need to find all unique links from all node configs
	allLinks := make(map[string]struct{})

	for _, nodeConfig := range clabernetesConfigs {
		topologyName := nodeConfig.Name

		for idx := range nodeConfig.Topology.Links {
			nadName := fmt.Sprintf("%s-l%d", topologyName, idx)
			allLinks[nadName] = struct{}{}
		}
	}

	allNADNames := make([]string, len(allLinks))
	var idx int
	for nadName := range allLinks {
		allNADNames[idx] = nadName
		idx++
	}

	nadDiffer.SetMissing(allNADNames)
	nadDiffer.SetExtra(allNADNames)

	return nadDiffer, nil
}

// RenderAll returns a slice of rendered NADs for the given topology.
func (r *NetworkAttachmentDefinitionReconciler) RenderAll(
	owningTopology *clabernetesapisv1alpha1.Topology,
	nadNames []string,
) []*unstructured.Unstructured {
	nads := make([]*unstructured.Unstructured, len(nadNames))

	for idx, nadName := range nadNames {
		nads[idx] = r.Render(
			owningTopology,
			nadName,
		)
	}

	return nads
}

// Render returns a rendered NAD for the given topology/link.
func (r *NetworkAttachmentDefinitionReconciler) Render(
	owningTopology *clabernetesapisv1alpha1.Topology,
	nadName string,
) *unstructured.Unstructured {
	owningTopologyName := owningTopology.GetName()

	annotations, globalLabels := r.configManagerGetter().GetAllMetadata()

	labels := map[string]string{
		clabernetesconstants.LabelApp:           clabernetesconstants.Clabernetes,
		clabernetesconstants.LabelName:          nadName,
		clabernetesconstants.LabelTopologyOwner: owningTopologyName,
		clabernetesconstants.LabelTopologyKind:  GetTopologyKind(owningTopology),
	}

	for k, v := range globalLabels {
		labels[k] = v
	}

	// basic netkit config
	config := map[string]any{
		"cniVersion": "0.3.1",
		"name":       nadName,
		"type":       "netkit",
		"mode":       "ptp",
	}

	configBytes, _ := json.Marshal(config)

	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "k8s.cni.cncf.io",
		Version: "v1",
		Kind:    "NetworkAttachmentDefinition",
	})
	nad.SetName(nadName)
	nad.SetNamespace(owningTopology.GetNamespace())
	nad.SetAnnotations(annotations)
	nad.SetLabels(labels)

	err := unstructured.SetNestedField(nad.Object, string(configBytes), "spec", "config")
	if err != nil {
		r.log.Criticalf("failed setting nad config, error: %s", err)
	}

	return nad
}

// Conforms checks if the existing NAD conforms to the rendered expectation.
func (r *NetworkAttachmentDefinitionReconciler) Conforms(
	existingNAD,
	renderedNAD *unstructured.Unstructured,
	expectedOwnerUID apimachinerytypes.UID,
) bool {
	existingSpec, _, _ := unstructured.NestedString(existingNAD.Object, "spec", "config")
	renderedSpec, _, _ := unstructured.NestedString(renderedNAD.Object, "spec", "config")

	if !reflect.DeepEqual(existingSpec, renderedSpec) {
		return false
	}

	if !clabernetesutilkubernetes.ExistingMapStringStringContainsAllExpectedKeyValues(
		existingNAD.GetAnnotations(),
		renderedNAD.GetAnnotations(),
	) {
		return false
	}

	if !clabernetesutilkubernetes.ExistingMapStringStringContainsAllExpectedKeyValues(
		existingNAD.GetLabels(),
		renderedNAD.GetLabels(),
	) {
		return false
	}

	ownerRefs := existingNAD.GetOwnerReferences()
	if len(ownerRefs) != 1 {
		return false
	}

	if ownerRefs[0].UID != expectedOwnerUID {
		return false
	}

	return true
}
