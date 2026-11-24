package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
)

// installationSettings contains only public scalar chart facts. Never add raw
// values, runtime defaults, environment, credentials or mounted file contents.
type installationSettings struct {
	PodDefaults     bool     `json:"pod_defaults"`
	LinkLocalFilter bool     `json:"link_local_filter"`
	PodSecurity     string   `json:"pod_security"`
	BundledStore    bool     `json:"bundled_store"`
	HTTPSNodePort   int      `json:"https_node_port"`
	SSHNodePort     int      `json:"ssh_node_port"`
	SourceImage     string   `json:"source_image"`
	ControllerImage string   `json:"controller_image"`
	ExecutorImage   string   `json:"executor_image"`
	StoreImage      string   `json:"store_image"`
	DataSize        string   `json:"data_size"`
	TmpSize         string   `json:"tmp_size"`
	Chosen          []string `json:"chosen"`
}

// effectiveSettings is an allowlist, not a serializer of serverConfig. In
// particular a future config field cannot silently become a status field.
func (a serverAdministration) effectiveSettings(ctx context.Context, parts []apicontract.PartStatus) []apicontract.EffectiveSetting {
	rows := settingsRows{chosen: a.owner.c.config.Installation.Chosen}
	rows.server(a.owner.c.config, a.owner.settings)
	rows.runCap(ctx, a.owner)
	rows.pins(a.owner.settings.SourcePolicy)
	rows.observations(ctx, a, parts)
	rows.installation(a.owner.c.config.Installation)
	return rows.values
}

type settingsRows struct {
	values []apicontract.EffectiveSetting
	chosen []string
}

func (s *settingsRows) add(name string, value, baseline any) {
	source := "default"
	if fmt.Sprint(value) != fmt.Sprint(baseline) || slices.Contains(s.chosen, name) {
		source = "chosen"
	}
	s.values = append(s.values, apicontract.EffectiveSetting{Name: name, Value: fmt.Sprint(value), Source: source})
}

func (s *settingsRows) server(c serverConfig, r serverRuntime) {
	s.add("version", binaryVersion(), binaryVersion())
	s.add("commit", commit, commit)
	s.add("build date", date, date)
	s.add("Git binary", r.GitBinary, "/usr/bin/git")
	s.add("database file", c.DatabaseFile, "/var/lib/oberth/oberth.db")
	s.add("namespace", c.Namespace, "oberth")
	s.add("pipeline namespace", c.PipelineNamespace, c.Namespace+"-runs")
	s.add("controller namespace", c.ArgoNamespace, c.Namespace)
	s.add("server URL", c.Public.URL, c.Public.URL)
	s.add("SSH port", c.Public.SSHPort, 30022)
	s.add("HTTPS listen", c.Listen.HTTPS, ":8443")
	s.add("SSH listen", c.Listen.SSH, ":2222")
	s.add("source listen", c.Listen.Source, ":8445")
	s.add("health listen", c.Listen.Health, ":8081")
	s.add("log bytes per run", c.Logs.RunLimitBytes, 67108864)
	s.add("schedule entries", c.Schedules.MaxEntries, 256)
	s.add("assist TTL seconds", c.Assist.TTLSeconds, 1200)
	s.add("source timeout seconds", r.SourceTimeoutSeconds, 60)
	s.add("admission timeout seconds", r.AdmissionTimeoutSeconds, 30)
	s.add("source client burst", r.SourceClientBurst, 96)
	s.add("receive input bytes", r.ReceiveMaxInputBytes, 1073741824)
	s.add("default branch", r.DefaultBranch, "refs/heads/main")
	s.add("image prefixes", r.ImagePrefixes, []string{})
	s.add("issue stale seconds", r.IssueStaleSeconds, 86400)
	s.add("active issue stale seconds", r.IssueActiveStaleSeconds, 7200)
	s.add("workflow defaults digest", r.Defaults.Digest, "")
	s.add("server image", r.HelperImage, r.HelperImage)
}

func (s *settingsRows) pins(policy json.RawMessage) {
	var pins map[string][]string
	if json.Unmarshal(policy, &pins) == nil {
		count := 0
		for _, producers := range pins {
			count += len(producers)
		}
		s.add("source pins", count, 0)
	} else {
		s.add("source pins", "unavailable", 0)
	}
}

func (s *settingsRows) observations(ctx context.Context, a serverAdministration, parts []apicontract.PartStatus) {
	seal := "unavailable"
	for _, p := range parts {
		if p.Name == "secret store" {
			seal = p.DisplayReason
			if p.Ready {
				seal = "unsealed"
			}
		}
	}
	s.add("store state", seal, "unsealed")
	s.add("scheduled backup", "off", "off")
	if a.owner.c.store != nil {
		stats, err := a.owner.c.store.InstallationStats(ctx)
		if err == nil {
			s.add("registered upstreams", stats.Upstreams, 0)
			s.add("registered repositories", stats.Repositories, 0)
			last := "never"
			if !stats.LastBackup.IsZero() {
				last = stats.LastBackup.UTC().Format(time.RFC3339Nano)
			}
			s.add("last backup", last, "never")
		} else {
			for _, name := range []string{"registered upstreams", "registered repositories", "last backup"} {
				s.add(name, "unavailable", "")
			}
		}
	}
}

func (rows *settingsRows) installation(s installationSettings) {
	rows.add("Pod defaults", onOff(s.PodDefaults), "on")
	rows.add("link-local egress filter", onOff(s.LinkLocalFilter), "on")
	rows.add("Pod Security", s.PodSecurity, "baseline")
	mode := "external"
	if s.BundledStore {
		mode = "bundled"
	}
	rows.add("store mode", mode, "bundled")
	rows.add("HTTPS node port", s.HTTPSNodePort, 30443)
	rows.add("SSH node port", s.SSHNodePort, 30022)
	rows.add("source plugin image", s.SourceImage, s.SourceImage)
	rows.add("Argo controller image", s.ControllerImage, s.ControllerImage)
	image, _, _ := strings.Cut(s.ControllerImage, "@")
	controllerVersion := image[strings.LastIndex(image, ":")+1:]
	rows.add("Argo controller version", controllerVersion, controllerVersion)
	rows.add("Argo executor image", s.ExecutorImage, s.ExecutorImage)
	if s.BundledStore {
		rows.add("store image", s.StoreImage, s.StoreImage)
	} else {
		rows.add("store image", "external", "")
	}
	rows.add("data volume size", s.DataSize, "10Gi")
	rows.add("temporary volume size", s.TmpSize, "2Gi")
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}
