package installer

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const foreignReview = "Helm revision %d: changed outside Oberth; use reviewed values."

// Detail exposes all reviewed public values separately from the compact approval gate.
func (p *Plan) Detail() []string {
	values, _ := inputs(p.release.chart, p.request.Values, p.choices.Namespace)
	raw, _ := json.MarshalIndent(map[string]any{
		"context": p.choices.Context, "cluster_address": p.address, "cluster_uid": p.uid,
		"images": reviewImages(values),
		"values": values, "failed_revision_values": p.history.Latest.Values,
		"foreign_revisions": p.history.Foreign,
	}, "", "  ")
	return strings.Split(string(raw), "\n")
}

func reviewImages(values map[string]any) map[string]string {
	return map[string]string{
		"server": reviewImage(values["imageRepository"], values["imageDigest"]),
		"source": reviewImage(values["sourcePluginImageRepository"], values["sourcePluginImageDigest"]),
	}
}

func reviewImage(repository, digest any) string {
	if digest == "unset" {
		return fmt.Sprint(repository) + ":dev"
	}
	return fmt.Sprintf("%v@%v", repository, digest)
}

func review(p *Plan, release string) []string {
	https, ssh := reviewEndpoints(p)
	values, _ := inputs(p.release.chart, p.request.Values, p.choices.Namespace)
	lines := []string{
		"Context: " + p.choices.Context,
		"Cluster: " + p.address + " (UID: " + p.uid + ")",
		"Namespaces: " + p.choices.Namespace + " / " + p.choices.PipelineNamespace + "; Pod Security: " + p.podSecurity,
		https, ssh,
		"Release: " + release,
		reviewResources(p, values),
	}
	lines = append(lines, reviewReachability(p.choices.URL)...)
	lines = append(lines, recordedAnswerChanges(p)...)
	if len(p.definitions.Shared) > 0 {
		lines = append(lines, "Workflow definitions: reuse; owner "+strings.Join(p.definitions.Owners, ", "))
	}
	if p.portScope != "" {
		lines = append(lines, p.portScope)
	}
	if p.plugin != "" {
		lines = append(lines, "Login plugin: "+p.plugin)
	}
	before := merge(p.release.chart.Values, p.history.Choices.Values)
	for _, setting := range []struct{ label, section, key, flag string }{
		{"Parallel runs", "runs", "parallel", "--parallel N"},
		{"Baseline Pod defaults", "policies", "floor", "--no-pod-defaults"},
		{"Link-local egress filter", "policies", "egress", "--no-link-local-filter"},
		{"Bundled store", "secretStore", "bundled", "--external-store URL"},
	} {
		value := settingText(values, setting.section, setting.key)
		old := settingText(before, setting.section, setting.key)
		if p.history.Deployed.Number > 0 && old != value {
			value = old + " -> " + value
		}
		lines = append(lines, setting.label+": "+value+" ("+setting.flag+")")
	}
	lines = append(lines, reviewSourcePins(p.history.Choices.Values, values, p.history.Deployed.Number > 0)...)
	lines = append(lines, "Profile: "+p.directory,
		"Files: installation.json, admin key/cert, CA, known_hosts, age/unseal keys",
		"Admin: "+p.admin.action)
	lines = append(lines, reviewHistory(p)...)
	lines = append(lines, StoreSnapshotHint)
	return append(lines, "Egress requires NetworkPolicy enforcement. Full values: --detail")
}

func reviewHistory(p *Plan) []string {
	var lines []string
	if p.history.Choices.Number > 0 {
		lines = append(lines, fmt.Sprintf("Saved choices: revision %d (%s)", p.history.Choices.Number, p.history.Choices.Status))
	}
	if p.history.Latest.Status == "uninstalled" {
		lines = append(lines, fmt.Sprintf("Removed revision %d", p.history.Latest.Number))
	} else if p.request.Recovery != nil {
		lines = append(lines, fmt.Sprintf("Interrupted revision %d: retry with this chart", p.request.Recovery.Pending))
	} else if p.history.Latest.Status == "failed" {
		lines = append(lines, fmt.Sprintf("Last revision %d failed; use installer choices", p.history.Latest.Number))
	}
	if !p.release.Verified() {
		lines = append(lines, "unverified local chart")
	}
	if len(p.history.Foreign) > 0 {
		lines = append(lines, fmt.Sprintf(foreignReview, p.history.Foreign[len(p.history.Foreign)-1]))
	}
	return lines
}
func settingText(values map[string]any, section, key string) string {
	block, _ := values[section].(map[string]any)
	if section == "secretStore" && key == "bundled" && block[key] == false {
		return "external " + fmt.Sprint(block["address"])
	}
	if value, ok := block[key].(bool); ok {
		if value {
			return "on"
		}
		return "off"
	}
	return fmt.Sprint(block[key])
}

func publicHost(address string) string {
	u, _ := url.Parse(address)
	return u.Hostname()
}

func reviewEndpoints(p *Plan) (string, string) {
	old := p.choices
	if hasRevisions(p.history) {
		old = deployedChoices(Choices{}, p.history.Choices.Values)
		if p.history.Choices.Number == 0 && p.history.Latest.Installer() {
			old = deployedChoices(Choices{}, p.history.Latest.Values)
		}
	}
	return fmt.Sprintf("HTTPS: %s -> node port %s", changedAnswer(old.URL, p.choices.URL), changedAnswer(old.HTTPSNodePort, p.choices.HTTPSNodePort)),
		fmt.Sprintf("SSH: %s:%s -> node port %s", changedAnswer(publicHost(old.URL), publicHost(p.choices.URL)), changedAnswer(old.SSHPort, p.choices.SSHPort), changedAnswer(old.SSHNodePort, p.choices.SSHNodePort))
}
func reviewResources(p *Plan, values map[string]any) string {
	action := "Create/update: "
	if hasRevisions(p.history) {
		action = "Update existing " + p.choices.Namespace + "/oberth: "
	}
	resources := "server, data PVC and policies, controller"
	if settingText(values, "secretStore", "bundled") == "on" {
		resources += ", store"
	}
	return action + resources
}
func changedAnswer(old, next any) string {
	if old != next {
		return fmt.Sprintf("%v -> %v", old, next)
	}
	return fmt.Sprint(next)
}

func reviewReachability(address string) []string {
	host := publicHost(address)
	if loopbackHost(host) {
		return []string{"Loopback address: only this machine can reach it."}
	}
	return nil
}
