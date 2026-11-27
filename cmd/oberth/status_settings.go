package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

// installationSettings contains only public scalar chart facts. Never add raw
// values, runtime defaults, environment, credentials or mounted file contents.
type installationSettings struct {
	PodDefaults     bool              `json:"pod_defaults"`
	LinkLocalFilter bool              `json:"link_local_filter"`
	PodSecurity     string            `json:"pod_security"`
	BundledStore    bool              `json:"bundled_store"`
	HTTPSNodePort   int               `json:"https_node_port"`
	SSHNodePort     int               `json:"ssh_node_port"`
	SourceImage     string            `json:"source_image"`
	ControllerImage string            `json:"controller_image"`
	ExecutorImage   string            `json:"executor_image"`
	StoreImage      string            `json:"store_image"`
	DataSize        string            `json:"data_size"`
	TmpSize         string            `json:"tmp_size"`
	Origins         map[string]string `json:"origins"`
}

// effectiveSettings is an allowlist, not a serializer of serverConfig. In
// particular a future config field cannot silently become a status field.
func (a serverAdministration) effectiveSettings(ctx context.Context, parts []apicontract.PartStatus, observations []apicontract.EffectiveSetting) []apicontract.EffectiveSetting {
	rows := settingsRows{origins: a.owner.c.config.Installation.Origins}
	rows.server(a.owner.c.config, a.owner.settings)
	rows.values = append(rows.values, observations...)
	rows.pins(a.owner.settings.SourcePolicy)
	rows.observations(ctx, a, parts)
	rows.installation(a.owner.c.config.Installation)
	return rows.values
}

type settingsRows struct {
	values  []apicontract.EffectiveSetting
	origins map[string]string
}

func (s *settingsRows) add(name string, value any) {
	source, known := s.origins[name]
	if !known {
		source = "default"
	}
	if source != "default" && source != "chosen" {
		source = ""
	}
	s.values = append(s.values, apicontract.EffectiveSetting{Name: name, Value: fmt.Sprint(value), Source: source})
}

func (s *settingsRows) observe(name string, value any) {
	s.values = append(s.values, apicontract.EffectiveSetting{Name: name, Value: fmt.Sprint(value)})
}

func (s *settingsRows) server(c serverConfig, r serverRuntime) {
	s.add("version", binaryVersion())
	s.add("commit", commit)
	s.add("build date", date)
	s.add("Git binary", r.GitBinary)
	s.add(apicontract.SettingDatabaseFile, c.DatabaseFile)
	s.add(apicontract.SettingNamespace, c.Namespace)
	s.add(apicontract.SettingPipelineNamespace, c.PipelineNamespace)
	s.add("controller namespace", c.ArgoNamespace)
	s.add(apicontract.SettingServerURL, c.Public.URL)
	s.add(apicontract.SettingSSHPort, c.Public.SSHPort)
	s.add("HTTPS listen", c.Listen.HTTPS)
	s.add("SSH listen", c.Listen.SSH)
	s.add("source listen", c.Listen.Source)
	s.add("health listen", c.Listen.Health)
	s.add(apicontract.SettingLogBytes, c.Logs.RunLimitBytes)
	s.add(apicontract.SettingScheduleEntries, c.Schedules.MaxEntries)
	s.add(apicontract.SettingAssistTTL, c.Assist.TTLSeconds)
	s.add("source timeout seconds", r.SourceTimeoutSeconds)
	s.add("admission timeout seconds", r.AdmissionTimeoutSeconds)
	s.add("source client burst", r.SourceClientBurst)
	b, _ := sourcedelivery.ResolveBudgets(r.SourceDelivery)
	s.add("source delivery", fmt.Sprintf("exports=%d archives=%d transfers=%d auth=%d workflow_waiters=%d", b.Exports, b.Archives, b.Transfers, b.Auth, b.WorkflowWaiters))
	s.add("receive input bytes", r.ReceiveMaxInputBytes)
	s.add("default branch", r.DefaultBranch)
	s.add("image prefixes", r.ImagePrefixes)
	s.add("issue stale seconds", r.IssueStaleSeconds)
	s.add("active issue stale seconds", r.IssueActiveStaleSeconds)
	s.add("workflow defaults digest", r.Defaults.Digest)
	s.add("server image", r.HelperImage)
}

func (s *settingsRows) pins(policy json.RawMessage) {
	var pins map[string][]string
	if json.Unmarshal(policy, &pins) == nil {
		count := 0
		for _, producers := range pins {
			count += len(producers)
		}
		s.add(apicontract.SettingSourcePins, count)
	} else {
		s.add(apicontract.SettingSourcePins, "unavailable")
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
	s.observe("store state", seal)
	if a.owner.c.store != nil {
		stats, err := a.owner.c.store.InstallationStats(ctx)
		if err == nil {
			s.observe(apicontract.SettingRegisteredUpstreams, stats.Upstreams)
			s.observe(apicontract.SettingRegisteredRepositories, stats.Repositories)
			last := "never"
			if !stats.LastBackup.IsZero() {
				last = stats.LastBackup.UTC().Format(time.RFC3339Nano)
			}
			s.observe("last backup", last)
		} else {
			for _, name := range []string{apicontract.SettingRegisteredUpstreams, apicontract.SettingRegisteredRepositories, "last backup"} {
				s.observe(name, "unavailable")
			}
		}
	}
}

func (rows *settingsRows) installation(s installationSettings) {
	rows.add(apicontract.SettingPodDefaults, onOff(s.PodDefaults))
	rows.add(apicontract.SettingLinkLocalFilter, onOff(s.LinkLocalFilter))
	rows.add("Pod Security", s.PodSecurity)
	mode := "external"
	if s.BundledStore {
		mode = "bundled"
	}
	rows.add(apicontract.SettingStoreMode, mode)
	rows.add(apicontract.SettingHTTPSNodePort, s.HTTPSNodePort)
	rows.add(apicontract.SettingSSHNodePort, s.SSHNodePort)
	rows.add("source plugin image", s.SourceImage)
	rows.add("Argo controller image", s.ControllerImage)
	rows.add("Argo executor image", s.ExecutorImage)
	if s.BundledStore {
		rows.add("store image", s.StoreImage)
	} else {
		rows.add("store image", "external")
	}
	rows.add(apicontract.SettingDataSize, s.DataSize)
	rows.add(apicontract.SettingTmpSize, s.TmpSize)
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}
