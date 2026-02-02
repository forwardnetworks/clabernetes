package testhelper

import (
	"os/exec"
	"testing"
)

const (
	kubectl = "kubectl"
)

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

	if _, err := exec.LookPath(kubectl); err != nil {
		t.Skipf("skipping: kubectl not found in PATH: %v", err)
	}

	cmd := exec.CommandContext( //nolint:gosec
		t.Context(),
		kubectl,
		string(operation),
		"namespace",
		namespace,
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("skipping: kubectl is not usable (operation=%s namespace=%s): %v; output=%s", operation, namespace, err, out)
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

	if _, err := exec.LookPath(kubectl); err != nil {
		t.Skipf("skipping: kubectl not found in PATH: %v", err)
	}

	cmd := exec.CommandContext( //nolint:gosec
		t.Context(),
		kubectl,
		string(operation),
		"--namespace",
		namespace,
		"-f",
		fileName,
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("skipping: kubectl is not usable (operation=%s namespace=%s file=%s): %v; output=%s", operation, namespace, fileName, err, out)
	}
}

// KubectlGetOp runs get on the given object, returning the yaml output.
func KubectlGetOp(t *testing.T, kind, namespace, name string) []byte {
	t.Helper()

	if _, err := exec.LookPath(kubectl); err != nil {
		t.Skipf("skipping: kubectl not found in PATH: %v", err)
	}

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

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("skipping: kubectl is not usable (get %s/%s ns=%s): %v; output=%s", kind, name, namespace, err, out)
	}
	return out
}
