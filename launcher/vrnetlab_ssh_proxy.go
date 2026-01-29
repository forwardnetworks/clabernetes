package launcher

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	clabernetesconstants "github.com/srl-labs/clabernetes/constants"
	claberneteserrors "github.com/srl-labs/clabernetes/errors"
)

const (
	vrnetlabMgmtSSHAddr = "169.254.100.2:22"
	sshProxyListenAddr  = ":22"
	vrnetlabDialTimeout = 5 * time.Second
	proxyCopyDoneBuffer = 2
)

func (c *clabernetes) maybeStartVrnetlabSSHProxy() {
	if os.Getenv(clabernetesconstants.LauncherNativeModeEnv) != clabernetesconstants.True {
		return
	}

	kind := strings.ToLower(strings.TrimSpace(c.nodeKind))
	if kind != "cisco_iol" && kind != "cisco_ioll2" {
		return
	}

	// vrnetlab uses qemu user-space networking which does not handle CHECKSUM_PARTIAL / TSO/GSO
	// packets well on veth pairs. Disable offloads on the internal management veth to ensure TCP
	// (SSH) works reliably.
	//
	// Ensure the veth exists early (before vrnetlab starts) so we can disable offloads
	// deterministically. The vrnetlab bootstrap script is idempotent and will reuse the same
	// interface names.
	err := c.ensureVrnetlabMgmtVeth()
	if err != nil {
		c.logger.Warnf("failed ensuring vrnetlab mgmt veth: %v", err)
	}

	c.disableInterfaceOffloads("vrl-mgmt0")
	c.disableInterfaceOffloads("vrl-mgmt1")

	go func() {
		err := runTCPProxy(c.ctx, sshProxyListenAddr, vrnetlabMgmtSSHAddr)
		if err != nil {
			c.logger.Warnf("vrnetlab ssh proxy failed: %s", err)
		}
	}()

	c.logger.Infof("vrnetlab ssh proxy enabled: %s -> %s", sshProxyListenAddr, vrnetlabMgmtSSHAddr)
}

func (c *clabernetes) ensureVrnetlabMgmtVeth() error {
	// vrl-mgmt0 (host side): 169.254.100.1/30
	// vrl-mgmt1 (ios side): used by iouyap as Ethernet0/0
	const (
		hostDev   = "vrl-mgmt0"
		iosDev    = "vrl-mgmt1"
		hostCIDR  = "169.254.100.1/30"
		checkWait = 50 * time.Millisecond
		checkMax  = 40 // 2s total
	)

	// If already exists, just make sure it is up + has address.
	cmd := exec.CommandContext(c.ctx, "ip", "link", "show", "dev", hostDev)

	err := cmd.Run()
	if err != nil {
		createCmd := exec.CommandContext(
			c.ctx,
			"ip",
			"link",
			"add",
			hostDev,
			"type",
			"veth",
			"peer",
			"name",
			iosDev,
		)

		out, err2 := createCmd.CombinedOutput()
		if err2 != nil {
			c.logger.Warnf(
				"ip link add %s/%s failed: %s",
				hostDev,
				iosDev,
				strings.TrimSpace(string(out)),
			)

			return errors.Join(claberneteserrors.ErrConnectivity, err2)
		}
	}

	// Wait briefly for both ends to appear (race with kernel).
	for range checkMax {
		hostCmd := exec.CommandContext(c.ctx, "ip", "link", "show", "dev", hostDev)
		iosCmd := exec.CommandContext(c.ctx, "ip", "link", "show", "dev", iosDev)

		hostOK := hostCmd.Run() == nil
		iosOK := iosCmd.Run() == nil

		if hostOK && iosOK {
			break
		}

		time.Sleep(checkWait)
	}

	// Ensure address on hostDev (ignore if already exists).
	_ = exec.CommandContext(c.ctx, "ip", "addr", "add", hostCIDR, "dev", hostDev).Run()
	_ = exec.CommandContext(c.ctx, "ip", "link", "set", hostDev, "up").Run()
	_ = exec.CommandContext(c.ctx, "ip", "link", "set", iosDev, "up").Run()

	return nil
}

func (c *clabernetes) disableInterfaceOffloads(iface string) {
	if iface == "" {
		return
	}

	_, err := exec.LookPath("ethtool")
	if err != nil {
		c.logger.Warnf("ethtool not found; cannot disable offloads on %s", iface)

		return
	}

	// Best-effort: ethtool may return non-zero if the interface doesn't exist yet,
	// or if an offload is not supported. We don't want to fail the pod for that.
	cmd := exec.CommandContext(
		c.ctx,
		"ethtool",
		"-K",
		iface,
		"tx",
		"off",
		"tso",
		"off",
		"gso",
		"off",
		"gro",
		"off",
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		c.logger.Warnf(
			"failed disabling offloads on %s: %v (%s)",
			iface,
			err,
			strings.TrimSpace(string(out)),
		)

		return
	}

	c.logger.Infof("disabled offloads on %s", iface)
}

func runTCPProxy(ctx context.Context, listenAddr, targetAddr string) error {
	lc := net.ListenConfig{}

	ln, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		return errors.Join(claberneteserrors.ErrConnectivity, err)
	}

	defer func() { _ = ln.Close() }()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}

			// transient accept error
			continue
		}

		go handleProxyConn(ctx, conn, targetAddr)
	}
}

func handleProxyConn(ctx context.Context, inbound net.Conn, targetAddr string) {
	defer func() { _ = inbound.Close() }()

	d := net.Dialer{Timeout: vrnetlabDialTimeout}

	outbound, err := d.DialContext(ctx, "tcp", targetAddr)
	if err != nil {
		return
	}

	defer func() { _ = outbound.Close() }()

	// Bidirectional copy.
	done := make(chan struct{}, proxyCopyDoneBuffer)

	go func() {
		_, _ = io.Copy(outbound, inbound)

		done <- struct{}{}
	}()

	go func() {
		_, _ = io.Copy(inbound, outbound)

		done <- struct{}{}
	}()

	<-done
}
