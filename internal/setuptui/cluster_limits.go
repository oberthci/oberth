package setuptui

import (
	"errors"
	"strconv"
	"strings"
)

func networkPolicyQuestion() *question {
	return &question{
		id: "cluster.network", step: stepCluster, heading: "Restrict CI network egress?",
		explain: func(*WizardState) string {
			return "A NetworkPolicy firewalls what CI jobs reach. auto enforces it on a fresh install, k3s included, and keeps the live setting on a rerun."
		},
		options: func(*WizardState) []option {
			return []option{
				{value: "auto", label: "auto", detail: "enforce where the network plugin supports it"},
				{value: "true", label: "strict", detail: "always enforce"},
				{value: "false", label: "off", detail: "never enforce"},
			}
		},
		selected: func(st *WizardState) string {
			if st.Config.NetworkPolicy == "" {
				return "auto"
			}
			return st.Config.NetworkPolicy
		},
		save: func(st *WizardState, answer []string) error {
			np, ok := canonicalNetworkPolicy(answer[0])
			if !ok {
				return errors.New("network policy must be auto, strict, or off")
			}
			st.Config.NetworkPolicy = np
			return nil
		},
	}
}

func argoLimitsQuestion() *question {
	return &question{
		id: "cluster.argo-limits", step: stepCluster, heading: "Set Argo controller limits.",
		explain: func(*WizardState) string {
			return "Argo owns Workflow parallelism. Leave a field empty to preserve an existing release; new releases use parallelism 4 and namespaceParallelism 0. Zero means unlimited."
		},
		fields:       []string{"controller.parallelism", "controller.namespaceParallelism"},
		placeholders: []string{"new: 4; existing: preserve", "new: 0; existing: preserve"},
		values: func(st *WizardState) []string {
			out := []string{"", ""}
			for i, value := range []*int{st.Config.ArgoParallelism, st.Config.ArgoNamespaceParallelism} {
				if value != nil {
					out[i] = strconv.Itoa(*value)
				}
			}
			return out
		},
		save: func(st *WizardState, answer []string) error {
			values := []*int{nil, nil}
			for i, raw := range answer {
				if strings.TrimSpace(raw) == "" {
					continue
				}
				value, err := strconv.Atoi(strings.TrimSpace(raw))
				if err != nil || value < 0 {
					return errors.New("parallelism must be a non-negative integer, or empty to preserve the release")
				}
				values[i] = &value
			}
			st.Config.ArgoParallelism, st.Config.ArgoNamespaceParallelism = values[0], values[1]
			return nil
		},
	}
}
