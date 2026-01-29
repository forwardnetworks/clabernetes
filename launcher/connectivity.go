package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	clabernetesapisv1alpha1 "github.com/srl-labs/clabernetes/apis/v1alpha1"
	clabernetesconstants "github.com/srl-labs/clabernetes/constants"
	claberneteserrors "github.com/srl-labs/clabernetes/errors"
	claberneteslauncherconnectivity "github.com/srl-labs/clabernetes/launcher/connectivity"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultTunnelsFileName             = "node-tunnels.json"
	tunnelsDirPerm         os.FileMode = 0o750
	tunnelsFilePerm        os.FileMode = 0o600
	tunnelsCRFetchInterval             = 1 * time.Second
	tunnelsCRFetchTimeout              = 30 * time.Second
)

var errTunnelsCacheNotFound = errors.New("tunnels cache file not found")

func (c *clabernetes) connectivity() {
	tunnels, err := c.getTunnels()
	if err != nil {
		c.logger.Fatalf("failed loading tunnels content, err: %s", err)
	}

	connectivityManager, err := claberneteslauncherconnectivity.NewManager(
		c.ctx,
		nil,
		c.logger,
		c.kubeClabernetesClient,
		tunnels,
		os.Getenv(
			clabernetesconstants.LauncherConnectivityKind,
		),
	)
	if err != nil {
		c.logger.Fatalf("failed creating connectivity manager, err: %s", err)
	}

	connectivityManager.Run()
}

func (c *clabernetes) getTunnels() ([]*clabernetesapisv1alpha1.PointToPointTunnel, error) {
	// Prefer cached tunnels file if present (written by the init-container setup step).
	tunnels, err := c.getTunnelsFromFile()
	if err == nil {
		return tunnels, nil
	}

	nodeName := os.Getenv(clabernetesconstants.LauncherNodeNameEnv)

	ctx, cancel := context.WithTimeout(c.ctx, clientDefaultTimeout)
	defer cancel()

	tunnelsCR, err := c.kubeClabernetesClient.ClabernetesV1alpha1().Connectivities(
		os.Getenv(clabernetesconstants.PodNamespaceEnv),
	).Get(
		ctx,
		os.Getenv(
			clabernetesconstants.LauncherTopologyNameEnv,
		),
		metav1.GetOptions{},
	)
	if err != nil {
		return nil, err
	}

	nodeTunnels, ok := tunnelsCR.Spec.PointToPointTunnels[nodeName]
	if !ok {
		c.logger.Warnf(
			"no tunnels found for node %q, continuing but things may be broken",
			nodeName,
		)
	}

	return nodeTunnels, nil
}

func (c *clabernetes) getTunnelsFromFile() ([]*clabernetesapisv1alpha1.PointToPointTunnel, error) {
	p := runtimePath(defaultTunnelsFileName)

	//nolint:gosec // p is under the in-pod runtime dir; not user-controlled
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errTunnelsCacheNotFound, err)
	}

	if len(b) == 0 {
		return nil, fmt.Errorf("%w: empty file", errTunnelsCacheNotFound)
	}

	var tunnels []*clabernetesapisv1alpha1.PointToPointTunnel

	err = json.Unmarshal(b, &tunnels)
	if err != nil {
		return nil, fmt.Errorf("parse tunnels file %q: %w", p, err)
	}

	return tunnels, nil
}

func (c *clabernetes) cacheNodeTunnels() error {
	nodeName := os.Getenv(clabernetesconstants.LauncherNodeNameEnv)
	if nodeName == "" {
		return fmt.Errorf(
			"%w: missing %s",
			claberneteserrors.ErrInvalidData,
			clabernetesconstants.LauncherNodeNameEnv,
		)
	}

	p := runtimePath(defaultTunnelsFileName)

	// If the tunnels file already exists and is non-empty, do not attempt to overwrite it.
	//nolint:gosec // p is under the in-pod runtime dir; not user-controlled
	b, err := os.ReadFile(p)
	if err == nil && len(b) > 0 {
		c.logger.Debugf("tunnels file already present, skipping cache: %s", p)

		return nil
	}

	err = os.MkdirAll(filepath.Dir(p), tunnelsDirPerm)
	if err != nil {
		return fmt.Errorf("mkdir tunnels dir: %w", err)
	}

	ctx, cancel := context.WithTimeout(c.ctx, tunnelsCRFetchTimeout)
	defer cancel()

	var tunnelsCR *clabernetesapisv1alpha1.Connectivity

	// Wait briefly for the connectivity CR to exist (topology/controller race during init).
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for connectivity CR: %w", ctx.Err())
		default:
		}

		cr, getErr := c.kubeClabernetesClient.ClabernetesV1alpha1().Connectivities(
			os.Getenv(clabernetesconstants.PodNamespaceEnv),
		).Get(
			ctx,
			os.Getenv(clabernetesconstants.LauncherTopologyNameEnv),
			metav1.GetOptions{},
		)
		if getErr == nil {
			tunnelsCR = cr

			break
		}

		time.Sleep(tunnelsCRFetchInterval)
	}

	nodeTunnels, ok := tunnelsCR.Spec.PointToPointTunnels[nodeName]
	if !ok {
		c.logger.Warnf(
			"no tunnels found for node %q, continuing but things may be broken",
			nodeName,
		)

		nodeTunnels = []*clabernetesapisv1alpha1.PointToPointTunnel{}
	}

	b, err = json.Marshal(nodeTunnels)
	if err != nil {
		return fmt.Errorf("marshal tunnels: %w", err)
	}

	err = os.WriteFile(p, b, tunnelsFilePerm)
	if err != nil {
		return fmt.Errorf("write tunnels file: %w", err)
	}

	c.logger.Debugf("cached tunnels file: %s", p)

	return nil
}
