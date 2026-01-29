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

	img := strings.ToLower(strings.TrimSpace(c.imageName))

	// vrnetlab qemu-based images typically require /dev/kvm + /dev/net/tun and a privileged
	// container. IOL is the exception (runs a process + iouyap) and does not require KVM.
	if strings.Contains(img, "/vrnetlab/") && kind != "cisco_iol" && kind != "cisco_ioll2" {
		c.ensureKVM()
		c.ensureTun()

		return
	}
}

func (c *clabernetes) ensureKVM() {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		c.logger.Fatalf("native mode requires /dev/kvm for node kind %q: %s", c.nodeKind, err)
	}

	_ = f.Close()
}

func (c *clabernetes) ensureTun() {
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		c.logger.Fatalf("native mode requires /dev/net/tun for node kind %q: %s", c.nodeKind, err)
	}

	_ = f.Close()
}
