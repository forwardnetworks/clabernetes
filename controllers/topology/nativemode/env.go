package nativemode

import (
	"strings"

	k8scorev1 "k8s.io/api/core/v1"
)

func applyEnvMap(in *ApplyInput) {
	if in == nil || in.NodeDef == nil || len(in.NodeDef.Env) == 0 {
		return
	}

	for k, v := range in.NodeDef.Env {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}

		in.NOS.Env = append(in.NOS.Env, k8scorev1.EnvVar{Name: k, Value: v})
	}
}
