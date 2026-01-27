package launcher

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
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

	go func() {
		if err := runTCPProxy(c.ctx, sshProxyListenAddr, vrnetlabMgmtSSHAddr); err != nil {
			c.logger.Warnf("vrnetlab ssh proxy failed: %s", err)
		}
	}()
	c.logger.Infof("vrnetlab ssh proxy enabled: %s -> %s", sshProxyListenAddr, vrnetlabMgmtSSHAddr)
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
