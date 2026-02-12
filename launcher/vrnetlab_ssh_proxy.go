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
	vrnetlabMgmtSNMPAddr = "169.254.100.2:161"
	sshProxyListenAddr  = ":22"
	snmpProxyListenAddr = ":161"
)

func (c *clabernetes) maybeStartVrnetlabSSHProxy() {
	if os.Getenv(clabernetesconstants.LauncherNativeModeEnv) != clabernetesconstants.True {
		return
	}

	img := strings.ToLower(strings.TrimSpace(c.imageName))
	if img == "" || !strings.Contains(img, "/vrnetlab/") {
		return
	}

	// Skyforge default: do not bind a proxy on TCP/22 in the pod netns.
	//
	// Most vrnetlab images start QEMU with user networking + hostfwd rules that bind TCP/22
	// inside the pod netns. Any sidecar listener on :22 (including this proxy) can race and
	// cause QEMU to fail with:
	//   "Could not set up host forwarding rule 'tcp:0.0.0.0:22-...:22'"
	//
	// Operators can explicitly opt in to the proxy by setting SKYFORGE_VRNETLAB_SSH_PROXY=true.
	enableProxy := strings.EqualFold(strings.TrimSpace(os.Getenv("SKYFORGE_VRNETLAB_SSH_PROXY")), "true")

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

	if !enableProxy {
		return
	}

	// SNMP (UDP/161) is needed for performance collection, and unlike TCP/22 there
	// is no QEMU hostfwd port-binding race to worry about (hostfwd is TCP-only).
	//
	// Start the UDP proxy immediately so collectors can reach podIP:161 reliably.
	go func() {
		if err := runUDPProxy(c.ctx, snmpProxyListenAddr, vrnetlabMgmtSNMPAddr); err != nil {
			c.logger.Warnf("vrnetlab snmp proxy failed: %s", err)
		}
	}()
	c.logger.Infof("vrnetlab snmp proxy started: %s -> %s", snmpProxyListenAddr, vrnetlabMgmtSNMPAddr)

	go func() {
		// Many vrnetlab images start QEMU with user networking and bind hostfwd ports
		// (including TCP/22) inside the pod netns. If we bind :22 first, QEMU will fail
		// to start. Give the NOS/QEMU ample time to bind its ports first; if :22 is still
		// unused after that grace period, we fall back to a simple TCP proxy to the
		// internal mgmt veth.
		const (
			// Some NOS images take >60s before QEMU binds hostfwd ports (esp. with large
			// disk images and cold caches). If we start the proxy too early, QEMU fails
			// with "Could not set up host forwarding rule ... :22".
			defaultWaitBeforeProxy = 2 * time.Minute
			dialTimeout     = 200 * time.Millisecond
		)

		waitBeforeProxy := defaultWaitBeforeProxy
		// Cisco IOL is not QEMU-based; it won't bind port 22 in the pod netns. Waiting
		// 2 minutes here just makes deployments feel broken.
		if strings.Contains(img, "/cisco_iol:") || strings.Contains(img, "/cisco_iol@") || strings.Contains(img, "/cisco_iol") {
			waitBeforeProxy = 5 * time.Second
		}

		select {
		case <-time.After(waitBeforeProxy):
		case <-c.ctx.Done():
			return
		}

		d := net.Dialer{Timeout: dialTimeout}
		conn, err := d.DialContext(c.ctx, "tcp", "127.0.0.1:22")
		if err == nil {
			_ = conn.Close()
			c.logger.Infof("vrnetlab ssh proxy skipped: port 22 already in use by NOS/QEMU")
			return
		}

		if err := runTCPProxy(c.ctx, sshProxyListenAddr, vrnetlabMgmtSSHAddr); err != nil {
			c.logger.Warnf("vrnetlab ssh proxy failed: %s", err)
		}
	}()
	c.logger.Infof("vrnetlab ssh proxy armed (will start if port 22 is free): %s -> %s", sshProxyListenAddr, vrnetlabMgmtSSHAddr)
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

func runUDPProxy(ctx context.Context, listenAddr, targetAddr string) error {
	laddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("resolve udp listen %s: %w", listenAddr, err)
	}
	raddr, err := net.ResolveUDPAddr("udp", targetAddr)
	if err != nil {
		return fmt.Errorf("resolve udp target %s: %w", targetAddr, err)
	}

	ln, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return fmt.Errorf("listen udp %s: %w", listenAddr, err)
	}
	defer ln.Close()

	// Close the socket on shutdown so ReadFromUDP unblocks.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	buf := make([]byte, 64*1024)
	for {
		n, clientAddr, err := ln.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			continue
		}

		// Copy packet for the goroutine.
		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		go func(pkt []byte, clientAddr *net.UDPAddr) {
			c := net.Dialer{Timeout: 2 * time.Second}
			out, err := c.DialContext(ctx, "udp", raddr.String())
			if err != nil {
				return
			}
			defer out.Close()

			udpOut, ok := out.(*net.UDPConn)
			if ok {
				_ = udpOut.SetDeadline(time.Now().Add(3 * time.Second))
			}

			_, _ = out.Write(pkt)

			// Forward a single response datagram back to the client.
			resp := make([]byte, 64*1024)
			rn, err := out.Read(resp)
			if err != nil || rn <= 0 {
				return
			}
			_, _ = ln.WriteToUDP(resp[:rn], clientAddr)
		}(pkt, clientAddr)
	}
}
