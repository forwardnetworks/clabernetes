package connectivity

import (
	"context"
	"crypto/sha1"
	"fmt"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"time"

	clabernetesapisv1alpha1 "github.com/srl-labs/clabernetes/apis/v1alpha1"
	clabernetesconstants "github.com/srl-labs/clabernetes/constants"
	claberneteserrors "github.com/srl-labs/clabernetes/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	resolveServiceMaxAttempts = 5
	resolveServiceSleep       = 10 * time.Second
)

type vxlanManager struct {
	*common

	currentTunnels map[string]*clabernetesapisv1alpha1.PointToPointTunnel
}

func (m *vxlanManager) Run() {
	m.currentTunnels = make(map[string]*clabernetesapisv1alpha1.PointToPointTunnel)

	m.logger.Info(
		"connectivity mode is 'vxlan', setting up any required tunnels...",
	)

	for _, tunnel := range m.initialTunnels {
		err := m.runContainerlabVxlanToolsCreate(
			tunnel.LocalNode,
			tunnel.LocalInterface,
			tunnel.Destination,
			tunnel.TunnelID,
		)
		if err != nil {
			m.logger.Fatalf(
				"failed setting up tunnel to remote node '%s' for local interface '%s', error: %s",
				tunnel.RemoteNode,
				tunnel.LocalInterface,
				err,
			)
		}

		// we store them in a nice little map by local interface name so they're easy to
		// reconcile on connectivity cr updates
		m.currentTunnels[tunnel.LocalInterface] = tunnel
	}

	m.logger.Debug("initial vxlan tunnel creation complete")

	m.logger.Debug("start connectivity custom resource watch...")

	go watchConnectivity(
		m.ctx,
		m.logger,
		m.clabernetesClient,
		m.updateVxlanTunnels,
	)

	m.logger.Debug("vxlan connectivity setup complete")
}

func (m *vxlanManager) resolveVXLANService(vxlanRemote string) (string, error) {
	var resolvedVxlanRemotes []net.IP

	var err error

	for range resolveServiceMaxAttempts {
		resolvedVxlanRemotes, err = net.LookupIP(vxlanRemote) //nolint: noctx
		if err != nil {
			m.logger.Warnf(
				"failed resolving remote vxlan endpoint but under max attempts will try"+
					" again in %s. error: %s",
				resolveServiceSleep,
				err,
			)

			time.Sleep(resolveServiceSleep)

			continue
		}

		break
	}

	if len(resolvedVxlanRemotes) != 1 {
		// We've seen environments where the launcher's /etc/resolv.conf ends up without
		// working nameservers, causing DNS lookups to default to localhost and fail.
		// Fall back to resolving the ClusterIP/Endpoint via the Kubernetes API so vxlan
		// connectivity doesn't depend on in-container DNS.
		ip, kerr := resolveVXLANServiceViaKubeAPI(m.ctx, vxlanRemote)
		if kerr != nil {
			return "", fmt.Errorf(
				"%w: did not get exactly one ip resolved for remote vxlan endpoint (dns=%v, kube=%v)",
				claberneteserrors.ErrConnectivity,
				err,
				kerr,
			)
		}

		m.logger.Warnf("resolved remote vxlan endpoint via kubernetes api: %s -> %s", vxlanRemote, ip)

		return ip, nil
	}

	return resolvedVxlanRemotes[0].String(), nil
}

func resolveVXLANServiceViaKubeAPI(ctx context.Context, vxlanRemote string) (string, error) {
	serviceName, namespace := parseServiceFQDN(vxlanRemote)
	if serviceName == "" || namespace == "" {
		return "", fmt.Errorf("%w: could not parse service name/namespace from %q", claberneteserrors.ErrInvalidData, vxlanRemote)
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return "", err
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", err
	}

	svc, err := client.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	if svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != "None" {
		return svc.Spec.ClusterIP, nil
	}

	ep, err := client.CoreV1().Endpoints(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	for _, subset := range ep.Subsets {
		for _, addr := range subset.Addresses {
			if addr.IP != "" {
				return addr.IP, nil
			}
		}
	}

	return "", fmt.Errorf("%w: no endpoint addresses found for %s/%s", claberneteserrors.ErrConnectivity, namespace, serviceName)
}

func parseServiceFQDN(host string) (serviceName, namespace string) {
	parts := strings.Split(host, ".")
	if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1]
	}

	// Fall back to in-pod namespace (for short names like <svc> or <svc>.<ns>)
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		return "", ""
	}

	if len(parts) == 1 && parts[0] != "" {
		return parts[0], ns
	}

	return "", ""
}

func (m *vxlanManager) runContainerlabVxlanToolsCreate(
	localNodeName,
	cntLink,
	vxlanRemote string,
	vxlanID int,
) error {
	var err error
	resolvedVxlanRemote := vxlanRemote
	if net.ParseIP(vxlanRemote) == nil {
		ip, err := m.resolveVXLANService(vxlanRemote)
		if err != nil {
			return err
		}
		resolvedVxlanRemote = ip
	}

	m.logger.Debugf("resolved remote vxlan tunnel service address as '%s'", resolvedVxlanRemote)

	link := sanitizeLinuxIfName(cntLink)
	vxlanInterfaceName := fmt.Sprintf("%s-%s", localNodeName, link)
	m.logger.Debugf("Attempting to delete existing vxlan interface '%s'", vxlanInterfaceName)

	err = m.runContainerlabVxlanToolsDelete(m.ctx, localNodeName, link)
	if err != nil {
		m.logger.Warnf(
			"failed while deleting existing vxlan interface '%s', error: '%s'",
			vxlanInterfaceName,
			err,
		)
	}

	// In docker-mode, containerlab creates a veth pair per endpoint and names the "host side" of the
	// veth `<node>-<ifname>` (e.g. `forti1-eth1`) which the vxlan tools then attach to.
	//
	// In native-mode, we run the NOS container directly as a k8s container (no Docker-in-Docker),
	// so there is no containerlab veth wiring step that would normally create this link.
	//
	// Since all containers in a pod share the same network namespace, we can create the expected veth
	// pair in the pod netns: `<node>-<ifname>` <-> `<ifname>`.
	err = m.ensurePodLinkExists(m.ctx, localNodeName, link)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext( //nolint:gosec
		m.ctx,
		"containerlab",
		"tools",
		"vxlan",
		"create",
		"--remote",
		resolvedVxlanRemote,
		"--id",
		strconv.Itoa(vxlanID),
		"--link",
		fmt.Sprintf("%s-%s", localNodeName, link),
		"--port",
		strconv.Itoa(clabernetesconstants.VXLANServicePort),
	)

	m.logger.Debugf(
		"using following args for vxlan tunnel creation (via containerlab) '%s'", cmd.Args,
	)

	cmd.Stdout = m.logger
	cmd.Stderr = m.logger

	err = cmd.Run()
	if err != nil {
		return err
	}

	return nil
}

func (m *vxlanManager) ensurePodLinkExists(
	ctx context.Context,
	localNodeName string,
	cntLink string,
) error {
	hostSide := fmt.Sprintf("%s-%s", localNodeName, cntLink)

	// If the host-side link already exists, we're done.
	checkCmd := exec.CommandContext(ctx, "ip", "link", "show", hostSide) //nolint:gosec
	if err := checkCmd.Run(); err == nil {
		return nil
	}

	// If the container-side link exists, we shouldn't clobber it.
	checkCntCmd := exec.CommandContext(ctx, "ip", "link", "show", cntLink) //nolint:gosec
	if err := checkCntCmd.Run(); err == nil {
		return fmt.Errorf(
			"%w: expected vxlan link %q missing but interface %q already exists",
			claberneteserrors.ErrConnectivity,
			hostSide,
			cntLink,
		)
	}

	// Create veth pair and bring it up.
	addCmd := exec.CommandContext( //nolint:gosec
		ctx,
		"ip",
		"link",
		"add",
		hostSide,
		"type",
		"veth",
		"peer",
		"name",
		cntLink,
	)
	addCmd.Stdout = m.logger
	addCmd.Stderr = m.logger
	if err := addCmd.Run(); err != nil {
		return fmt.Errorf("%w: failed creating veth %q <-> %q: %s", claberneteserrors.ErrConnectivity, hostSide, cntLink, err)
	}

	upHostCmd := exec.CommandContext(ctx, "ip", "link", "set", hostSide, "up") //nolint:gosec
	upHostCmd.Stdout = m.logger
	upHostCmd.Stderr = m.logger
	if err := upHostCmd.Run(); err != nil {
		return fmt.Errorf("%w: failed bringing up %q: %s", claberneteserrors.ErrConnectivity, hostSide, err)
	}

	upCntCmd := exec.CommandContext(ctx, "ip", "link", "set", cntLink, "up") //nolint:gosec
	upCntCmd.Stdout = m.logger
	upCntCmd.Stderr = m.logger
	if err := upCntCmd.Run(); err != nil {
		return fmt.Errorf("%w: failed bringing up %q: %s", claberneteserrors.ErrConnectivity, cntLink, err)
	}

	return nil
}

func (m *vxlanManager) runContainerlabVxlanToolsDelete(
	ctx context.Context,
	localNodeName,
	cntLink string,
) error {
	cmd := exec.CommandContext( //nolint:gosec
		ctx,
		"containerlab",
		"tools",
		"vxlan",
		"delete",
		"--prefix",
		fmt.Sprintf("vx-%s-%s", localNodeName, cntLink),
	)

	m.logger.Debugf(
		"using following args for vxlan tunnel deletion (via containerlab) '%s'", cmd.Args,
	)

	cmd.Stdout = m.logger
	cmd.Stderr = m.logger

	err := cmd.Run()
	if err != nil {
		return err
	}

	return nil
}

func sanitizeLinuxIfName(raw string) string {
	// Linux interface names must be <= 15 bytes and cannot contain '/'.
	// Some netlab/containerlab templates use interface identifiers like "Ethernet0/1"
	// which are not valid Linux ifnames.
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

	sum := sha1.Sum([]byte(s)) //nolint:gosec // non-crypto identifier
	suffix := fmt.Sprintf("%x", sum[:3]) // 6 chars
	prefixLen := 15 - 1 - len(suffix)
	if prefixLen < 1 {
		return suffix[:15]
	}
	return s[:prefixLen] + "-" + suffix
}

func (m *vxlanManager) updateVxlanTunnels(
	tunnels []*clabernetesapisv1alpha1.PointToPointTunnel,
) {
	// start with deleting extraneous tunnels...
	for _, existingTunnel := range m.currentTunnels {
		var found bool

		for _, tunnel := range tunnels {
			if tunnel.LocalInterface == existingTunnel.LocalInterface {
				found = true

				break
			}
		}

		if found {
			// the existing tunnel (or rather its local interface) is represented in the "new"
			// tunnels, nothing to do here
			continue
		}

		err := m.runContainerlabVxlanToolsDelete(
			m.ctx,
			existingTunnel.LocalNode,
			existingTunnel.LocalInterface,
		)
		if err != nil {
			m.logger.Fatalf(
				"failed deleting extraneous tunnel to remote node '%s' for local interface '%s'"+
					", error: %s",
				existingTunnel.RemoteNode,
				existingTunnel.LocalInterface,
				err,
			)
		}
	}

	tunnelsToReCreate := make([]*clabernetesapisv1alpha1.PointToPointTunnel, 0)

	for _, tunnel := range tunnels {
		existingTunnel, ok := m.currentTunnels[tunnel.LocalInterface]
		if ok && reflect.DeepEqual(existingTunnel, tunnel) {
			// we've already got a tunnel setup for this interface, so we gotta check to see if our
			// previously setup destination is the same -- if "yes" we can skip doing anything to
			// this one.
			continue
		}

		if ok {
			// tunnel for this interface exists but isnt the same as our desired setup, delete the
			// old tunnel before we create the new one
			err := m.runContainerlabVxlanToolsDelete(
				m.ctx,
				tunnel.LocalNode,
				tunnel.LocalInterface,
			)
			if err != nil {
				m.logger.Fatalf(
					"failed deleting existing tunnel to remote node '%s' for local interface '%s'"+
						" before re-configuring, error: %s",
					tunnel.RemoteNode,
					tunnel.LocalInterface,
					err,
				)
			}
		}

		tunnelsToReCreate = append(tunnelsToReCreate, tunnel)
	}

	for _, tunnel := range tunnelsToReCreate {
		err := m.runContainerlabVxlanToolsCreate(
			tunnel.LocalNode,
			tunnel.LocalInterface,
			tunnel.Destination,
			tunnel.TunnelID,
		)
		if err != nil {
			m.logger.Fatalf(
				"failed setting up tunnel to remote node '%s' for local interface '%s', error: %s",
				tunnel.RemoteNode,
				tunnel.LocalInterface,
				err,
			)
		}
	}
}
