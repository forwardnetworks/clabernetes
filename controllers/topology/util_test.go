package topology

import (
	"testing"

	clabernetesapisv1alpha1 "github.com/srl-labs/clabernetes/apis/v1alpha1"
)

func TestResolveNativeMode_DefaultsTrue(t *testing.T) {
	topo := &clabernetesapisv1alpha1.Topology{}
	if got := ResolveNativeMode(topo); got != true {
		t.Fatalf("expected ResolveNativeMode(nil) == true, got %v", got)
	}
}

func TestResolveNativeMode_UsesExplicitValue(t *testing.T) {
	topoTrue := &clabernetesapisv1alpha1.Topology{}
	topoTrue.Spec.Deployment.NativeMode = ptrBool(true)
	if got := ResolveNativeMode(topoTrue); got != true {
		t.Fatalf("expected ResolveNativeMode(true) == true, got %v", got)
	}

	topoFalse := &clabernetesapisv1alpha1.Topology{}
	topoFalse.Spec.Deployment.NativeMode = ptrBool(false)
	if got := ResolveNativeMode(topoFalse); got != false {
		t.Fatalf("expected ResolveNativeMode(false) == false, got %v", got)
	}
}

func ptrBool(v bool) *bool {
	return &v
}

