package launcher

import (
	"os"
	"strings"
)

func (c *clabernetes) ensureNativeDeviceRequirements() {
	kind := strings.ToLower(strings.TrimSpace(c.nodeKind))
	if kind == "" {
		return
	}

	switch kind {
	case "vr-vmx":
		c.ensureKVM()
	default:
	}
}

func (c *clabernetes) ensureKVM() {
	f, err := os.Open("/dev/kvm")
	if err != nil {
		c.logger.Fatalf("native mode requires /dev/kvm for node kind %q: %s", c.nodeKind, err)
	}

	_ = f.Close()
}
