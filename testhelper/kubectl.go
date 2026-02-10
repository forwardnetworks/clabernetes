package testhelper

import (
	"os/exec"
	"testing"
)

const (
	kubectl = "kubectl"
)

func ensureKubectl(t *testing.T) {
	t.Helper()

	// These helpers are used by e2e tests. When running in environments without
	// a configured Kubernetes cluster (e.g. local unit test runs), skip cleanly
	// instead of hard failing.
	if _, err := exec.LookPath(kubectl); err != nil {
		t.Skipf("kubectl not available: %v", err)
	}

	// If kubectl can't reach a cluster/context, we can't run the e2e tests.
	cmd := exec.CommandContext(t.Context(), kubectl, "cluster-info") //nolint:gosec
	if err := cmd.Run(); err != nil {
		t.Skipf("kubectl cluster not available: %v", err)
	}
}

// Operation represents a kubectl operation type, i.e. apply or delete.
type Operation string

const (
	// Apply is the apply kubectl operation.
	Apply Operation = "apply"
	// Delete is the delete kubectl operation.
	Delete Operation = "delete"
	// Create is the create kubectl operation.
	Create Operation = "create"
	// Get is the get kubectl operation.
	Get Operation = "get"
)

func kubectlNamespace(t *testing.T, operation Operation, namespace string) {
	t.Helper()

	ensureKubectl(t)

	cmd := exec.CommandContext( //nolint:gosec
		t.Context(),
		kubectl,
		string(operation),
		"namespace",
		namespace,
	)

	err := cmd.Run()
	if err != nil {
		t.Fatalf("error executing kubectl command, error: '%s'", err)
	}
}

// KubectlCreateNamespace execs a kubectl create namespace.
func KubectlCreateNamespace(t *testing.T, namespace string) {
	t.Helper()

	kubectlNamespace(t, Create, namespace)
}

// KubectlDeleteNamespace execs a kubectl delete namespace.
func KubectlDeleteNamespace(t *testing.T, namespace string) {
	t.Helper()

	kubectlNamespace(t, Delete, namespace)
}

// KubectlFileOp execs a kubectl operation on a file (i.e. apply/delete).
func KubectlFileOp(t *testing.T, operation Operation, namespace, fileName string) {
	t.Helper()

	ensureKubectl(t)

	cmd := exec.CommandContext( //nolint:gosec
		t.Context(),
		kubectl,
		string(operation),
		"--namespace",
		namespace,
		"-f",
		fileName,
	)

	_ = Execute(t, cmd)
}

// KubectlGetOp runs get on the given object, returning the yaml output.
func KubectlGetOp(t *testing.T, kind, namespace, name string) []byte {
	t.Helper()

	ensureKubectl(t)

	cmd := exec.CommandContext( //nolint:gosec
		t.Context(),
		kubectl,
		string(Get),
		kind,
		"--namespace",
		namespace,
		name,
		"-o",
		"yaml",
	)

	return Execute(t, cmd)
}
