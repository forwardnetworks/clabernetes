package launcher

import (
	"math/rand"
	"os"
	"time"

	clabernetesconstants "github.com/srl-labs/clabernetes/constants"
	claberneteslogging "github.com/srl-labs/clabernetes/logging"
	clabernetesutil "github.com/srl-labs/clabernetes/util"
)

// StartClabernetesSetup runs the launcher "setup" phase. This is intended for use in an init
// container in native mode to capture runtime state (pod net snapshot, tunnels cache) before the
// NOS container starts and potentially mutates the shared pod netns.
func StartClabernetesSetup() {
	rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec

	claberneteslogging.InitManager()

	logManager := claberneteslogging.GetManager()

	clabernetesLogger := logManager.MustRegisterAndGetLogger(
		clabernetesconstants.Clabernetes,
		clabernetesutil.GetEnvStrOrDefault(
			clabernetesconstants.LauncherLoggerLevelEnv,
			clabernetesconstants.Info,
		),
	)

	containerlabLogger := logManager.MustRegisterAndGetLogger(
		"containerlab",
		clabernetesconstants.Info,
	)

	nodeLogger := logManager.MustRegisterAndGetLogger(
		"node",
		clabernetesconstants.Info,
	)

	ctx, cancel := clabernetesutil.SignalHandledContext(clabernetesLogger.Criticalf)

	c := &clabernetes{
		ctx:                   ctx,
		cancel:                cancel,
		kubeClabernetesClient: mustNewKubeClabernetesClient(clabernetesLogger),
		appName: clabernetesutil.GetEnvStrOrDefault(
			clabernetesconstants.AppNameEnv,
			clabernetesconstants.AppNameDefault,
		),
		nodeName:             os.Getenv(clabernetesconstants.LauncherNodeNameEnv),
		logger:               clabernetesLogger,
		containerlabLogger:   containerlabLogger,
		nodeLogger:           nodeLogger,
		imageName:            os.Getenv(clabernetesconstants.LauncherNodeImageEnv),
		imagePullThroughMode: os.Getenv(clabernetesconstants.LauncherImagePullThroughModeEnv),
	}

	c.setupOnly()
}
