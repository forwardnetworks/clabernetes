package launcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	clabernetesapisv1alpha1 "github.com/srl-labs/clabernetes/apis/v1alpha1"
	clabernetesconstants "github.com/srl-labs/clabernetes/constants"
	claberneteslauncherconnectivity "github.com/srl-labs/clabernetes/launcher/connectivity"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

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
	// This avoids relying on in-pod access to the Kubernetes API at runtime, which can be
	// disrupted by NOS containers in native mode (they may mutate routes on the shared pod netns).
	if tunnels, ok, err := c.getTunnelsFromFile(); err != nil {
		return nil, err
	} else if ok {
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

func (c *clabernetes) getTunnelsFromFile() ([]*clabernetesapisv1alpha1.PointToPointTunnel, bool, error) {
	p := os.Getenv(clabernetesconstants.LauncherTunnelsFileEnv)
	if p == "" {
		// Default to a path on the shared docker/EmptyDir volume.
		p = "/var/lib/docker/clabernetes/node-tunnels.json"
	}
	p = filepath.Clean(p)

	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read tunnels file %q: %w", p, err)
	}
	if len(b) == 0 {
		return nil, false, nil
	}

	var tunnels []*clabernetesapisv1alpha1.PointToPointTunnel
	if err := json.Unmarshal(b, &tunnels); err != nil {
		return nil, false, fmt.Errorf("parse tunnels file %q: %w", p, err)
	}
	return tunnels, true, nil
}

func (c *clabernetes) cacheNodeTunnels() error {
	nodeName := os.Getenv(clabernetesconstants.LauncherNodeNameEnv)
	if nodeName == "" {
		return nil
	}
	ns := os.Getenv(clabernetesconstants.PodNamespaceEnv)
	if ns == "" {
		return nil
	}
	topologyName := os.Getenv(clabernetesconstants.LauncherTopologyNameEnv)
	if topologyName == "" {
		return nil
	}

	p := os.Getenv(clabernetesconstants.LauncherTunnelsFileEnv)
	if p == "" {
		p = "/var/lib/docker/clabernetes/node-tunnels.json"
	}
	p = filepath.Clean(p)

	// If the tunnels file already exists and is non-empty, do not attempt to
	// talk to the Kubernetes API again. In native mode, NOS containers can mutate
	// routes on the shared pod netns which may break access to the Service CIDR.
	if b, err := os.ReadFile(p); err == nil {
		if len(b) > 0 {
			c.logger.Debugf("tunnels file already present, skipping cache: %s", p)
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("mkdir tunnels dir: %w", err)
	}

	// Connectivities are created by the controller shortly after the Topology CR is created.
	// In practice, launcher init containers can start before the connectivity controller has
	// written the Connectivities CR, so we retry briefly here.
	var tunnelsCR *clabernetesapisv1alpha1.Connectivity
	var lastErr error
	deadline := time.Now().Add(60 * time.Second)
	for {
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = fmt.Errorf("timeout")
			}
			return fmt.Errorf("get connectivity %s/%s: %w", ns, topologyName, lastErr)
		}
		ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
		cr, err := c.kubeClabernetesClient.ClabernetesV1alpha1().Connectivities(ns).Get(ctx, topologyName, metav1.GetOptions{})
		cancel()
		if err == nil && cr != nil {
			tunnelsCR = cr
			break
		}
		lastErr = err
		time.Sleep(1 * time.Second)
	}

	nodeTunnels, ok := tunnelsCR.Spec.PointToPointTunnels[nodeName]
	if !ok {
		c.logger.Warnf("no tunnels found for node %q, continuing but things may be broken", nodeName)
	}
	if nodeTunnels == nil {
		nodeTunnels = []*clabernetesapisv1alpha1.PointToPointTunnel{}
	}

	// Replace service FQDN destinations with endpoint IPs during init so runtime does not depend
	// on in-pod DNS or access to the Service CIDR. Native-mode NOS containers can mutate routes on
	// the shared pod netns, breaking DNS and kube API access inside the launcher.
	if err := resolveTunnelDestinationsToEndpointIPs(c.ctx, nodeTunnels); err != nil {
		return err
	}

	b, err := json.Marshal(nodeTunnels)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return err
	}
	c.logger.Debugf("cached tunnels file: %s", p)
	return nil
}

func resolveTunnelDestinationsToEndpointIPs(ctx context.Context, tunnels []*clabernetesapisv1alpha1.PointToPointTunnel) error {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}

	for _, t := range tunnels {
		if t == nil || t.Destination == "" {
			continue
		}
		if net.ParseIP(t.Destination) != nil {
			continue
		}
		ip, err := resolveServiceEndpointIP(ctx, client, t.Destination)
		if err != nil {
			return fmt.Errorf("resolve vxlan destination %q: %w", t.Destination, err)
		}
		t.Destination = ip
	}

	return nil
}

func resolveServiceEndpointIP(ctx context.Context, client *kubernetes.Clientset, fqdn string) (string, error) {
	parts := strings.Split(fqdn, ".")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("invalid service fqdn: %q", fqdn)
	}
	serviceName := parts[0]
	namespace := parts[1]

	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ep, err := client.CoreV1().Endpoints(namespace).Get(reqCtx, serviceName, metav1.GetOptions{})
		cancel()
		if err == nil && ep != nil {
			for _, subset := range ep.Subsets {
				for _, addr := range subset.Addresses {
					if addr.IP != "" {
						return addr.IP, nil
					}
				}
				for _, addr := range subset.NotReadyAddresses {
					if addr.IP != "" {
						return addr.IP, nil
					}
				}
			}
			lastErr = fmt.Errorf("no endpoint addresses found for %s/%s", namespace, serviceName)
		} else {
			lastErr = err
		}

		time.Sleep(1 * time.Second)
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return "", lastErr
}
