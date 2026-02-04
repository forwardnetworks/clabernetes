package connectivity

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
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
)

const (
	resolveServiceMaxAttempts = 5
	resolveServiceSleep       = 10 * time.Second
	linuxIfNameMaxLen         = 15
	vxlanIfPrefix             = "vx-"
	vxlanHostSideMaxLen       = linuxIfNameMaxLen - len(vxlanIfPrefix) // keep room for vxlan tools interface names
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
		return "", fmt.Errorf(
			"%w: did not get exactly one ip resolved for remote vxlan endpoint",
			claberneteserrors.ErrConnectivity,
		)
	}

	return resolvedVxlanRemotes[0].String(), nil
}

func (m *vxlanManager) runContainerlabVxlanToolsCreate(
	localNodeName,
	cntLink,
	vxlanRemote string,
	vxlanID int,
) error {
	resolvedVxlanRemote, err := m.resolveVXLANService(vxlanRemote)
	if err != nil {
		return err
	}

	m.logger.Debugf("resolved remote vxlan tunnel service address as '%s'", resolvedVxlanRemote)

	link := sanitizeLinuxIfName(cntLink)
	hostSide := vxlanHostSideLinkName(localNodeName, link)
	vxlanInterfaceName := fmt.Sprintf("%s%s", vxlanIfPrefix, hostSide)
	m.logger.Debugf("Attempting to delete existing vxlan interface '%s'", vxlanInterfaceName)

	err = m.runContainerlabVxlanToolsDelete(m.ctx, hostSide)
	if err != nil {
		m.logger.Warnf(
			"failed while deleting existing vxlan interface '%s', error: '%s'",
			vxlanInterfaceName,
			err,
		)
	}

	if os.Getenv(clabernetesconstants.LauncherNativeModeEnv) == clabernetesconstants.True {
		// In docker mode, containerlab creates a veth pair per endpoint and names the "host side"
		// of the veth `<node>-<ifname>` (e.g. `forti1-eth1`) which the vxlan tools then attach to.
		//
		// In native mode, we run the NOS container directly as a k8s container (no DIND), so there
		// is no containerlab veth wiring step that would normally create this link.
		//
		// Since all containers in a pod share the same network namespace, we can create the
		// expected veth pair in the pod netns: `<node>-<ifname>` <-> `<ifname>`.
		err = m.ensurePodLinkExists(m.ctx, hostSide, link)
		if err != nil {
			return err
		}
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
		hostSide,
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
	hostSide string,
	cntLink string,
) error {
	// If the host-side link already exists, we're done.
	//nolint:gosec // hostSide is derived from sanitized interface names
	checkCmd := exec.CommandContext(ctx, "ip", "link", "show", hostSide)

	err := checkCmd.Run()
	if err == nil {
		return nil
	}

	// If the container-side link exists, we shouldn't clobber it.
	checkCntCmd := exec.CommandContext(ctx, "ip", "link", "show", cntLink)

	err = checkCntCmd.Run()
	if err == nil {
		return fmt.Errorf(
			"%w: expected vxlan link %q missing but interface %q already exists",
			claberneteserrors.ErrConnectivity,
			hostSide,
			cntLink,
		)
	}

	//nolint:gosec // hostSide/cntLink are derived from sanitized interface names
	addCmd := exec.CommandContext(
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

	err = addCmd.Run()
	if err != nil {
		return errors.Join(
			claberneteserrors.ErrConnectivity,
			fmt.Errorf("failed creating veth %q <-> %q: %w", hostSide, cntLink, err),
		)
	}

	//nolint:gosec // hostSide is derived from sanitized interface names
	upHostCmd := exec.CommandContext(ctx, "ip", "link", "set", hostSide, "up")
	upHostCmd.Stdout = m.logger
	upHostCmd.Stderr = m.logger

	err = upHostCmd.Run()
	if err != nil {
		return errors.Join(
			claberneteserrors.ErrConnectivity,
			fmt.Errorf("failed bringing up %q: %w", hostSide, err),
		)
	}

	upCntCmd := exec.CommandContext(ctx, "ip", "link", "set", cntLink, "up")
	upCntCmd.Stdout = m.logger
	upCntCmd.Stderr = m.logger

	err = upCntCmd.Run()
	if err != nil {
		return errors.Join(
			claberneteserrors.ErrConnectivity,
			fmt.Errorf("failed bringing up %q: %w", cntLink, err),
		)
	}

	return nil
}

func (m *vxlanManager) runContainerlabVxlanToolsDelete(
	ctx context.Context,
	hostSide string,
) error {
	cmd := exec.CommandContext( //nolint:gosec
		ctx,
		"containerlab",
		"tools",
		"vxlan",
		"delete",
		"--prefix",
		fmt.Sprintf("%s%s", vxlanIfPrefix, hostSide),
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

func vxlanHostSideLinkName(localNodeName, cntLink string) string {
	base := fmt.Sprintf("%s-%s", localNodeName, cntLink)
	if len(base) <= vxlanHostSideMaxLen {
		return base
	}

	sum := sha1.Sum([]byte(base))
	hash := hex.EncodeToString(sum[:])
	if len(hash) < vxlanHostSideMaxLen {
		// should never happen, but be defensive.
		return hash
	}

	node := sanitizeLinuxIfName(localNodeName)
	if node == "" {
		node = "n"
	}
	if len(node) > 3 {
		node = node[:3]
	}

	link := cntLink
	if link == "" {
		link = "l"
	}
	if len(link) > 3 {
		link = link[:3]
	}

	remain := vxlanHostSideMaxLen - (len(node) + len(link))
	if remain <= 0 {
		out := node + link
		if len(out) > vxlanHostSideMaxLen {
			out = out[:vxlanHostSideMaxLen]
		}
		return out
	}

	out := node + link + hash[:remain]
	if len(out) > vxlanHostSideMaxLen {
		out = out[:vxlanHostSideMaxLen]
	}
	return out
}

func sanitizeLinuxIfName(raw string) string {
	// Linux interface names must be <= 15 bytes and cannot contain '/'.
	s := strings.TrimSpace(raw)
	if s == "" {
		return "link"
	}

	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ":", "-")
	s = strings.ReplaceAll(s, " ", "-")

	b := make([]byte, 0, len(s))
	for i := range len(s) {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z',
			ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9',
			ch == '_' || ch == '-' || ch == '.':
			b = append(b, ch)
		default:
			// drop
		}
	}

	out := string(b)
	if out == "" {
		out = "link"
	}

	if len(out) > linuxIfNameMaxLen {
		out = out[:linuxIfNameMaxLen]
	}

	return strings.ToLower(out)
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

		link := sanitizeLinuxIfName(existingTunnel.LocalInterface)
		hostSide := vxlanHostSideLinkName(existingTunnel.LocalNode, link)
		err := m.runContainerlabVxlanToolsDelete(m.ctx, hostSide)
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
			link := sanitizeLinuxIfName(tunnel.LocalInterface)
			hostSide := vxlanHostSideLinkName(tunnel.LocalNode, link)
			err := m.runContainerlabVxlanToolsDelete(m.ctx, hostSide)
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
