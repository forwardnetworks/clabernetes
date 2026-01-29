//go:build linux
// +build linux

package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	clabernetesconstants "github.com/srl-labs/clabernetes/constants"
)

const (
	podNetGuardianTickInterval = 2 * time.Second

	podNetSnapshotDirPerm  os.FileMode = 0o750
	podNetSnapshotFilePerm os.FileMode = 0o600
)

var errPodNetSnapshotMissing = errors.New("pod net snapshot missing")

type podNetSnapshot struct {
	Interface string        `json:"interface"`
	Addrs     []podNetAddr  `json:"addrs"`
	Routes    []podNetRoute `json:"routes"`
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

type ipAddrJSON struct {
	IfName   string       `json:"ifname"`
	AddrInfo []ipAddrInfo `json:"addr_info"`
}

type ipAddrInfo struct {
	Family    string `json:"family"`
	Local     string `json:"local"`
	PrefixLen int    `json:"prefixlen"`
}

type ipRouteJSON struct {
	Dst     string   `json:"dst"`
	Gateway string   `json:"gateway"`
	Dev     string   `json:"dev"`
	Scope   string   `json:"scope"`
	Flags   []string `json:"flags"`
	Metric  int      `json:"metric"`
}

func (c *clabernetes) podNetSnapshotPath() string {
	return runtimePath("podnet.json")
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

	p := c.podNetSnapshotPath()

	if err := os.MkdirAll(filepath.Dir(p), podNetSnapshotDirPerm); err != nil {
		return fmt.Errorf("mkdir podnet snapshot dir: %w", err)
	}

	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal podnet snapshot: %w", err)
	}

	if err := os.WriteFile(p, b, podNetSnapshotFilePerm); err != nil {
		return fmt.Errorf("write podnet snapshot: %w", err)
	}

	c.logger.Debugf("captured pod net snapshot: %s", p)

	return nil
}

func (c *clabernetes) ensurePodNetFromSnapshot(ctx context.Context) error {
	if os.Getenv(clabernetesconstants.LauncherNativeModeEnv) != clabernetesconstants.True {
		return nil
	}

	p := c.podNetSnapshotPath()

	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("%w: %w", errPodNetSnapshotMissing, err)
	}

	if len(b) == 0 {
		return fmt.Errorf("%w: empty file", errPodNetSnapshotMissing)
	}

	var snap podNetSnapshot

	if err := json.Unmarshal(b, &snap); err != nil {
		return fmt.Errorf("parse pod net snapshot %s: %w", p, err)
	}

	// Best-effort repair: restore eth0 addresses and key routes that Kubernetes/CNI put in place.
	// Some NOS containers mutate routes/addresses in the shared pod netns; without these routes,
	// clabernetes connectivity (vxlan/slurpeeth) cannot reach remote tunnel endpoints.
	if err := applyPodNetSnapshot(ctx, &snap); err != nil {
		return err
	}

	return nil
}

func (c *clabernetes) startPodNetGuardian() {
	if os.Getenv(clabernetesconstants.LauncherNativeModeEnv) != clabernetesconstants.True {
		return
	}

	// Apply once immediately, then periodically.
	_ = c.ensurePodNetFromSnapshot(c.ctx)

	t := time.NewTicker(podNetGuardianTickInterval)
	defer t.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			_ = c.ensurePodNetFromSnapshot(c.ctx)
		}
	}
}

func readPodNetSnapshotFromKernel(
	ctx context.Context,
	ifname string,
) (*podNetSnapshot, error) {
	addrOut, err := exec.CommandContext(ctx, "ip", "-j", "addr", "show", "dev", ifname).
		Output()
		//nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read ip addr %s: %w", ifname, err)
	}

	routeOut, err := exec.CommandContext(ctx, "ip", "-j", "route", "show", "table", "main").
		Output()
		//nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read ip route: %w", err)
	}

	var addrItems []ipAddrJSON
	if err := json.Unmarshal(addrOut, &addrItems); err != nil {
		return nil, fmt.Errorf("parse ip addr json: %w", err)
	}

	var routeItems []ipRouteJSON
	if err := json.Unmarshal(routeOut, &routeItems); err != nil {
		return nil, fmt.Errorf("parse ip route json: %w", err)
	}

	snap := &podNetSnapshot{
		Interface: ifname,
		Addrs:     []podNetAddr{},
		Routes:    []podNetRoute{},
	}

	for _, item := range addrItems {
		for _, info := range item.AddrInfo {
			if info.Local == "" || (info.Family != "inet" && info.Family != "inet6") {
				continue
			}

			snap.Addrs = append(snap.Addrs, podNetAddr{
				Family:    info.Family,
				Local:     info.Local,
				PrefixLen: info.PrefixLen,
			})
		}
	}

	for _, r := range routeItems {
		dst := r.Dst
		if dst == "" {
			dst = "default"
		}

		// Preserve only eth0 routes and defaults; avoid capturing host-only routes added by NOS.
		if r.Dev != "" && r.Dev != ifname {
			continue
		}

		snap.Routes = append(snap.Routes, podNetRoute{
			Dst:     dst,
			Gateway: r.Gateway,
			Dev:     r.Dev,
			Scope:   r.Scope,
			Flags:   r.Flags,
			Metric:  r.Metric,
		})
	}

	return snap, nil
}

func applyPodNetSnapshot(
	ctx context.Context,
	snap *podNetSnapshot,
) error {
	if snap == nil || snap.Interface == "" {
		return nil
	}

	if err := exec.CommandContext(ctx, "ip", "link", "set", snap.Interface, "up").Run(); err != nil { //nolint:gosec
		return fmt.Errorf("ip link set %s up: %w", snap.Interface, err)
	}

	for _, a := range snap.Addrs {
		if a.Local == "" || a.PrefixLen <= 0 {
			continue
		}

		addr := fmt.Sprintf("%s/%d", a.Local, a.PrefixLen)
		familyFlag := "-4"
		if a.Family == "inet6" {
			familyFlag = "-6"
		}
		cmd := exec.CommandContext( //nolint:gosec
			ctx,
			"ip",
			familyFlag,
			"addr",
			"replace",
			addr,
			"dev",
			snap.Interface,
		)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf(
				"ip %s addr replace %s dev %s: %w",
				familyFlag,
				addr,
				snap.Interface,
				err,
			)
		}
	}

	// Best-effort: restore default/eth0 routes.
	for _, r := range snap.Routes {
		if r.Dst == "" {
			continue
		}

		args := []string{"route", "replace", r.Dst}
		if r.Gateway != "" {
			args = append(args, "via", r.Gateway)
		}
		if r.Dev != "" {
			args = append(args, "dev", r.Dev)
		}
		if r.Metric > 0 {
			args = append(args, "metric", fmt.Sprintf("%d", r.Metric))
		}

		cmd := exec.CommandContext(ctx, "ip", args...) //nolint:gosec
		_ = cmd.Run()
	}

	return nil
}
