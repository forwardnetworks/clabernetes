package launcher

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	clabernetesconstants "github.com/srl-labs/clabernetes/constants"
)

const (
	vrnetlabMgmtSSHAddr = "169.254.100.2:22"
	sshProxyListenAddr  = ":22"
)

func (c *clabernetes) maybeStartVrnetlabSSHProxy() {
	if os.Getenv(clabernetesconstants.LauncherNativeModeEnv) != clabernetesconstants.True {
		return
	}

	img := strings.ToLower(strings.TrimSpace(c.imageName))
	if img == "" || !strings.Contains(img, "/vrnetlab/") {
		return
	}

	// vrnetlab uses iouyap/qemu user-space networking which does not handle
	// CHECKSUM_PARTIAL / TSO/GSO packets well on veth pairs. Disable offloads on
	// the internal management veth to ensure TCP (SSH) works reliably.
	//
	// Ensure the veth exists early (before vrnetlab starts) so we can disable
	// offloads deterministically. The vrnetlab bootstrap script is idempotent and
	// will reuse the same interface names.
	if err := c.ensureVrnetlabMgmtVeth(); err != nil {
		c.logger.Warnf("failed ensuring vrnetlab mgmt veth: %v", err)
	}
	c.disableInterfaceOffloads("vrl-mgmt0")
	c.disableInterfaceOffloads("vrl-mgmt1")

	go func() {
		if err := runTCPProxy(c.ctx, sshProxyListenAddr, vrnetlabMgmtSSHAddr); err != nil {
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
	if err := exec.CommandContext(c.ctx, "ip", "link", "show", "dev", hostDev).Run(); err != nil {
		if out, err2 := exec.CommandContext(c.ctx, "ip", "link", "add", hostDev, "type", "veth", "peer", "name", iosDev).CombinedOutput(); err2 != nil {
			return fmt.Errorf("ip link add %s/%s: %v (%s)", hostDev, iosDev, err2, strings.TrimSpace(string(out)))
		}
	}

	// Wait briefly for both ends to appear (race with kernel).
	for i := 0; i < checkMax; i++ {
		hostOK := exec.CommandContext(c.ctx, "ip", "link", "show", "dev", hostDev).Run() == nil
		iosOK := exec.CommandContext(c.ctx, "ip", "link", "show", "dev", iosDev).Run() == nil
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
		c.logger.Warnf("failed disabling offloads on %s: %v (%s)", iface, err, strings.TrimSpace(string(out)))
		return
	}
	c.logger.Infof("disabled offloads on %s", iface)
}

func runTCPProxy(ctx context.Context, listenAddr, targetAddr string) error {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	defer ln.Close()

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
	defer inbound.Close()

	d := net.Dialer{Timeout: 5 * time.Second}
	outbound, err := d.DialContext(ctx, "tcp", targetAddr)
	if err != nil {
		return
	}
	defer outbound.Close()

	// Bidirectional copy.
	done := make(chan struct{}, 2)
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
