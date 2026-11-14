package installer

import (
	"fmt"
	"strings"
	"testing"
)

func TestTemplateDefaultContainerSecurityFloor(t *testing.T) {
	for _, tc := range []struct {
		name, template string
		refused        bool
	}{
		{"privileged", `"container":{"securityContext":{"privileged":true}}`, true},
		{"escalation", `"script":{"securityContext":{"allowPrivilegeEscalation":true}}`, true},
		{"capability", `"container":{"securityContext":{"capabilities":{"add":["SYS_ADMIN"]}}}`, true},
		{"sidecar-missing-context", `"sidecars":[{"name":"helper","image":"image"}]`, true},
		{"init-missing-context", `"initContainers":[{"name":"helper","image":"image"}]`, true},
		{"container-set-missing-context", `"containerSet":{"containers":[{"name":"helper","image":"image"}]}`, true},
		{"safe-partial", `"container":{"securityContext":{"capabilities":{"add":["NET_BIND_SERVICE"]}}}`, false},
		{"safe-sidecar", `"sidecars":[{"name":"helper","image":"image","securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := fmt.Sprintf(`{"controller":{"workflowDefaults":{"spec":{"templateDefaults":{%s}}}}}`, tc.template)
			_, err := prepareArgoSecurity(t.Context(), fixwaveSecurityDeps(t, true, stored), DefaultArgoNamespace, true, false, nil)
			if (err != nil) != tc.refused || (err != nil && !strings.Contains(err.Error(), "templateDefaults")) {
				t.Fatal(err)
			}
		})
	}
}
