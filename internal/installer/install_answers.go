package installer

import (
	"fmt"
	"net/url"
	"strings"
)

func (p Profile) answersOnly() bool {
	return p.BootstrapPending && p.AdminKey == "" && p.AdminCert == "" && p.KnownHosts == "" && p.PipelineNamespace != "" && p.HTTPSNodePort != 0 && p.SSHNodePort != 0
}

func (p Profile) answers() map[string]any {
	return map[string]any{"pipeline": map[string]any{"namespace": p.PipelineNamespace}, "nodePorts": map[string]any{"https": p.HTTPSNodePort, "ssh": p.SSHNodePort}}
}

func (e *Engine) saveInitialAnswers(p *Plan) error {
	if p.history.Deployed.Number != 0 {
		return nil
	}
	old, err := e.Credentials.profile()
	if err == nil && !old.answersOnly() && hasRevisions(p.history) {
		return nil
	}
	if err != nil && !isAbsent(err) {
		return problem("custody", err)
	}
	record := Profile{Version: 1, Context: p.choices.Context, ClusterUID: p.uid, Namespace: p.choices.Namespace, PipelineNamespace: p.choices.PipelineNamespace, HTTPSNodePort: p.choices.HTTPSNodePort, SSHNodePort: p.choices.SSHNodePort, BootstrapPending: true}
	return e.Credentials.saveProfile(record, p.admin.id)
}

func (e *Engine) installAnswers(c Choices, h History) (map[string]any, error) {
	if strings.HasPrefix(h.Latest.Status, "pending-") && h.Latest.Installer() {
		return e.pendingInstallAnswers(c, h)
	}
	if h.Deployed.Number != 0 {
		return h.Choices.Values, nil
	}
	record, err := e.Credentials.profile()
	if isAbsent(err) {
		return h.Choices.Values, nil
	}
	if err != nil {
		return nil, problem("custody", err)
	}
	if record.PipelineNamespace == "" {
		return h.Choices.Values, nil
	}
	if hasRevisions(h) {
		if err = checkResumeAnswers(c, record, h); err != nil {
			return nil, err
		}
	}
	return merge(h.Choices.Values, record.answers()), nil
}

func checkResumeAnswers(c Choices, record Profile, h History) error {
	saved := deployedChoices(Choices{}, record.answers())
	if c.PipelineNamespace != "" && c.PipelineNamespace != saved.PipelineNamespace {
		return problem("pipeline-change", nil)
	}
	if changedPorts(c, saved) {
		return problem("install-answers", nil)
	}
	if strings.HasPrefix(h.Latest.Status, "pending-") && h.Latest.Installer() {
		pending := deployedChoices(Choices{}, h.Latest.Values)
		if pending.PipelineNamespace != saved.PipelineNamespace || pending.HTTPSNodePort != saved.HTTPSNodePort || pending.SSHNodePort != saved.SSHNodePort {
			return problem("install-answers", nil)
		}
	}
	return nil
}

func changedPorts(c, saved Choices) bool {
	return c.HTTPSNodePort != 0 && c.HTTPSNodePort != saved.HTTPSNodePort || c.SSHNodePort != 0 && c.SSHNodePort != saved.SSHNodePort
}

func (e *Engine) installChoices(c Choices, h History) (Choices, error) {
	stored, err := e.installAnswers(c, h)
	if err != nil {
		return c, err
	}
	if hasRevisions(h) {
		if err = checkPipelineChoice(c, stored); err != nil {
			return c, err
		}
	}
	endpoint, _ := url.Parse(e.Cluster.Config.Host)
	return resolve(deployedChoices(c, stored), endpoint.Hostname())
}

func hasRevisions(h History) bool { return h.Latest.Number != 0 || h.Deployed.Number != 0 }

func recordedAnswerChanges(p *Plan) []string {
	if hasRevisions(p.history) || p.previous.Namespace == "" {
		return nil
	}
	var changes []string
	for _, pair := range []struct {
		name      string
		old, next any
	}{
		{"Server namespace", p.previous.Namespace, p.choices.Namespace},
		{"Pipeline namespace", p.previous.PipelineNamespace, p.choices.PipelineNamespace},
		{"HTTPS node port", p.previous.HTTPSNodePort, p.choices.HTTPSNodePort},
		{"SSH node port", p.previous.SSHNodePort, p.choices.SSHNodePort},
	} {
		if pair.old != pair.next {
			changes = append(changes, fmt.Sprintf("%s: %v -> %v", pair.name, pair.old, pair.next))
		}
	}
	return changes
}

func (e *Engine) pendingInstallAnswers(c Choices, h History) (map[string]any, error) {
	record, err := e.Credentials.profile()
	if err != nil && !isAbsent(err) {
		return nil, problem("custody", err)
	}
	if err == nil && record.PipelineNamespace != "" {
		if err = checkResumeAnswers(c, record, h); err != nil {
			return nil, err
		}
	}
	pending := deployedChoices(Choices{}, h.Latest.Values)
	if changedPorts(c, pending) || c.PipelineNamespace != "" && c.PipelineNamespace != pending.PipelineNamespace {
		return nil, problem("install-answers", nil)
	}
	return h.Latest.Values, nil
}
