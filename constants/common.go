package constants

// Version of the clabernetes manager. Set with build flags, so leave at 0.0.0.
var Version = "0.0.0" //nolint: gochecknoglobals

const (
	// Clabernetes is the name... clabernetes.
	Clabernetes = "clabernetes"

	// Clabverter is a constant for the lovely name "clabverter".
	Clabverter = "clabverter"

	// True is a constant representing the string "true".
	True = "true"

	// False is a constant representing the string "false".
	False = "false"

	// Default is a constant for the string default -- often used for keys in clabernetes maps.
	Default = "default"

	// AppNameDefault is the default name for the "app" (the helm value appName) -- "clabernetes".
	AppNameDefault = "clabernetes"

	// VXLANServicePort is the UDP destination port used for per-link VXLAN tunnels.
	//
	// NOTE: We intentionally use 6784, as Arista cEOS installs a restrictive default
	// iptables policy and explicitly allows a small set of UDP ports including
	// 4784/6784/7784. Using one of these avoids needing any NOS-specific firewall
	// changes to support VXLAN-based link emulation.
	VXLANServicePort = 6784

	// SlurpeethServicePort is the port number for slurpeeth that we use in the kubernetes service.
	SlurpeethServicePort = 4799

	// TCP is... TCP.
	TCP = "TCP"

	// UDP is... UDP.
	UDP = "UDP"

	// FileModeRead is "read". Used for configmap mount permissions in the
	// TopologySpec/FilesFromConfigMap.
	FileModeRead = "read"

	// FileModeExecute is "execute". Used for configmap mount permissions in the
	// TopologySpec/FilesFromConfigMap.
	FileModeExecute = "execute"

	// HostKeyword is the containerlab reserved keyword to define host links endpoints.
	HostKeyword = "host"
)
