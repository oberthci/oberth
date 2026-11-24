package api_test

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"strings"
)

func exampleSettingsStatus() apicontract.SystemStatus {
	values := [][2]string{
		{"version", "1"}, {"commit", strings.Repeat("a", 40)},
		{"parallel runs", "4"}, {"Pod defaults", "on"}, {"link-local egress filter", "on"}, {"Pod Security", "baseline"},
		{"store mode", "bundled"}, {"store state", "unsealed"}, {"scheduled backup", "off"}, {"last backup", "2026-11-16T14:00:00Z"},
		{"source pins", "2"}, {"registered upstreams", "2"}, {"registered repositories", "3"},
		{"server URL", "https://ci.example:30443"}, {"SSH port", "30022"}, {"HTTPS node port", "30443"}, {"SSH node port", "30022"},
		{"server image", "registry.example/oberth@sha256:" + strings.Repeat("a", 64)},
		{"source plugin image", "registry.example/source-plugin@sha256:" + strings.Repeat("b", 64)},
		{"Argo controller image", "quay.io/argoproj/workflow-controller:v4.0.15@sha256:" + strings.Repeat("c", 64)},
		{"Argo executor image", "quay.io/argoproj/argoexec:v4.0.15@sha256:" + strings.Repeat("d", 64)},
		{"store image", "docker.io/openbao/openbao:2.7.2@sha256:" + strings.Repeat("e", 64)},
		{"namespace", "oberth"}, {"pipeline namespace", "oberth-runs"}, {"controller namespace", "oberth"},
		{"HTTPS listen", ":8443"}, {"SSH listen", ":2222"}, {"source listen", ":8445"}, {"health listen", ":8081"},
		{"log bytes per run", "67108864"}, {"schedule entries", "256"}, {"assist TTL seconds", "1200"},
		{"source timeout seconds", "60"}, {"admission timeout seconds", "30"}, {"source client burst", "96"},
		{"receive input bytes", "1073741824"}, {"default branch", "refs/heads/main"}, {"image prefixes", "[]"},
		{"issue stale seconds", "86400"}, {"active issue stale seconds", "7200"}, {"workflow defaults digest", ""},
		{"data volume size", "10Gi"}, {"temporary volume size", "2Gi"},
	}
	status := apicontract.SystemStatus{Ready: true, Version: "1"}
	settings := []apicontract.EffectiveSetting{}
	for i, pair := range values {
		source := "default"
		if i%3 == 0 {
			source = "chosen"
		}
		switch pair[0] {
		case "store state", "last backup", "registered upstreams", "registered repositories":
			source = ""
		}
		settings = append(settings, apicontract.EffectiveSetting{Name: pair[0], Value: pair[1], Source: source})
	}
	status.Settings = &settings
	return status
}
