package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	clabernetesconstants "github.com/srl-labs/clabernetes/constants"
)

// podNetSnapshotPath is stored on the shared EmptyDir volume (mounted at /var/lib/docker in our
// Skyforge deployment), so it is writable both by the init-container setup step and the launcher
// container at runtime.
const podNetSnapshotPath = "/var/lib/docker/clabernetes/podnet.json"

type podNetSnapshot struct {
	Interface string            `json:"interface"`
	Addrs     []podNetAddr      `json:"addrs"`
	Routes    []podNetRoute     `json:"routes"`
	Meta      map[string]string `json:"meta,omitempty"`
}

type podNetAddr struct {
	Family    string `json:"family"`
	Local     string `json:"local"`
	PrefixLen int    `json:"prefixLen"`
}

type podNetRoute struct {
	Dst     string   `json:"dst"`
	Gateway string   `json:"gateway,omitempty"`
	Dev     string   `json:"dev,omitempty"`
	Scope   string   `json:"scope,omitempty"`
	Flags   []string `json:"flags,omitempty"`
	Metric  int      `json:"metric,omitempty"`
}

func (c *clabernetes) capturePodNetSnapshot(ctx context.Context) error {
	if os.Getenv(clabernetesconstants.LauncherNativeModeEnv) != clabernetesconstants.True {
		return nil
	}

	snap, err := readPodNetSnapshotFromKernel(ctx, "eth0")
	if err != nil {
		return err
	}
	if snap == nil {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(podNetSnapshotPath), 0o755); err != nil {
		return fmt.Errorf("mkdir podnet snapshot dir: %w", err)
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal podnet snapshot: %w", err)
	}
	if err := os.WriteFile(podNetSnapshotPath, b, 0o644); err != nil {
		return fmt.Errorf("write podnet snapshot: %w", err)
	}

	c.logger.Debugf("captured pod net snapshot: %s", podNetSnapshotPath)
	return nil
}

func (c *clabernetes) ensurePodNetFromSnapshot(ctx context.Context) {
	if os.Getenv(clabernetesconstants.LauncherNativeModeEnv) != clabernetesconstants.True {
		return
	}

	b, err := os.ReadFile(podNetSnapshotPath)
	if err != nil {
		return
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return
	}

	var snap podNetSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		c.logger.Warnf("failed parsing pod net snapshot %s: %s", podNetSnapshotPath, err)
		return
	}

	// Best-effort repair: restore eth0 addresses and key routes that Kubernetes/CNI put in place.
	// Some NOS containers (notably cEOS) mutate routes in the shared pod netns; without these
	// routes, clabernetes connectivity (vxlan/slurpeeth) cannot reach remote tunnel endpoints.
	if err := applyPodNetSnapshot(ctx, &snap); err != nil {
		c.logger.Warnf("failed applying pod net snapshot: %s", err)
	}
}

func (c *clabernetes) startPodNetGuardian() {
	if os.Getenv(clabernetesconstants.LauncherNativeModeEnv) != clabernetesconstants.True {
		return
	}

	// Apply once immediately, then periodically.
	c.ensurePodNetFromSnapshot(c.ctx)

	t := time.NewTicker(2 * time.Second)
	defer t.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			c.ensurePodNetFromSnapshot(c.ctx)
		}
	}
}

func readPodNetSnapshotFromKernel(ctx context.Context, ifname string) (*podNetSnapshot, error) {
	addrOut, err := exec.CommandContext(ctx, "ip", "-j", "addr", "show", "dev", ifname).Output() //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read ip addr %s: %w", ifname, err)
	}

	routeOut, err := exec.CommandContext(ctx, "ip", "-j", "route", "show", "table", "main").Output() //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read ip route: %w", err)
	}

	var addrAny []map[string]any
	if err := json.Unmarshal(addrOut, &addrAny); err != nil {
		return nil, fmt.Errorf("parse ip addr json: %w", err)
	}
	var routeAny []map[string]any
	if err := json.Unmarshal(routeOut, &routeAny); err != nil {
		return nil, fmt.Errorf("parse ip route json: %w", err)
	}

	snap := &podNetSnapshot{
		Interface: ifname,
		Addrs:     []podNetAddr{},
		Routes:    []podNetRoute{},
		Meta:      map[string]string{},
	}

	for _, item := range addrAny {
		infosAny, ok := item["addr_info"].([]any)
		if !ok {
			continue
		}
		for _, infoAny := range infosAny {
			info, ok := infoAny.(map[string]any)
			if !ok {
				continue
			}
			fam, _ := info["family"].(string)
			local, _ := info["local"].(string)
			prefix, _ := info["prefixlen"].(float64)
			if local == "" || (fam != "inet" && fam != "inet6") {
				continue
			}
			snap.Addrs = append(snap.Addrs, podNetAddr{
				Family:    fam,
				Local:     local,
				PrefixLen: int(prefix),
			})
		}
	}

	for _, r := range routeAny {
		dst, _ := r["dst"].(string)
		gw, _ := r["gateway"].(string)
		dev, _ := r["dev"].(string)
		scope, _ := r["scope"].(string)
		var flags []string
		if flagsAny, ok := r["flags"].([]any); ok {
			for _, fAny := range flagsAny {
				if f, ok := fAny.(string); ok && f != "" {
					flags = append(flags, f)
				}
			}
		}
		metric := 0
		if mAny, ok := r["metric"].(float64); ok {
			metric = int(mAny)
		}

		// Preserve only eth0 routes and defaults; avoid capturing host-only routes added by NOS.
		if dev != "" && dev != ifname {
			continue
		}
		if dst == "" {
			dst = "default"
		}

		snap.Routes = append(snap.Routes, podNetRoute{
			Dst:     dst,
			Gateway: gw,
			Dev:     dev,
			Scope:   scope,
			Flags:   flags,
			Metric:  metric,
		})
	}

	return snap, nil
}

func applyPodNetSnapshot(ctx context.Context, snap *podNetSnapshot) error {
	if snap == nil || snap.Interface == "" {
		return nil
	}

	// Ensure interface exists and is up.
	if err := exec.CommandContext(ctx, "ip", "link", "set", snap.Interface, "up").Run(); err != nil { //nolint:gosec
		return fmt.Errorf("ip link set %s up: %w", snap.Interface, err)
	}

	// Restore addresses.
	for _, a := range snap.Addrs {
		if a.Local == "" || a.PrefixLen == 0 {
			continue
		}
		addr := fmt.Sprintf("%s/%d", a.Local, a.PrefixLen)
		// "replace" is idempotent for our use and handles the case where NOS flushed addresses.
		cmd := exec.CommandContext(ctx, "ip", "addr", "replace", addr, "dev", snap.Interface) //nolint:gosec
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ip addr replace %s dev %s: %w", addr, snap.Interface, err)
		}
	}

	// Restore routes.
	//
	// Important: on /32 pod IP setups (including our Cilium config), the default route is valid only
	// after a link-scope host route to the gateway exists (ip -j route show: a route with dst=<gw>,
	// dev=eth0, scope=link). If we try to restore the default route first, "ip route replace default
	// via <gw> dev eth0" fails with exit status 2 ("invalid gateway").
	//
	// To avoid that, restore "no gateway" routes first, then routes that include a gateway.
	noGatewayRoutes := make([]podNetRoute, 0, len(snap.Routes))
	withGatewayRoutes := make([]podNetRoute, 0, len(snap.Routes))
	for _, r := range snap.Routes {
		if r.Dst == "" {
			continue
		}
		if r.Gateway == "" {
			noGatewayRoutes = append(noGatewayRoutes, r)
		} else {
			withGatewayRoutes = append(withGatewayRoutes, r)
		}
	}

	applyRoute := func(r podNetRoute) error {
		args := []string{"route", "replace", r.Dst}
		if r.Gateway != "" {
			args = append(args, "via", r.Gateway)
		}
		args = append(args, "dev", snap.Interface)
		if r.Scope != "" {
			args = append(args, "scope", r.Scope)
		}
		if r.Metric > 0 {
			args = append(args, "metric", fmt.Sprintf("%d", r.Metric))
		}
		for _, f := range r.Flags {
			if f == "onlink" {
				args = append(args, "onlink")
				break
			}
		}
		cmd := exec.CommandContext(ctx, "ip", args...) //nolint:gosec
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ip %v: %w", args, err)
		}
		return nil
	}

	for _, r := range noGatewayRoutes {
		if err := applyRoute(r); err != nil {
			return err
		}
	}
	for _, r := range withGatewayRoutes {
		if err := applyRoute(r); err != nil {
			return err
		}
	}

	return nil
}
