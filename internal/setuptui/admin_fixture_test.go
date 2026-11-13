package setuptui

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/oberthci/oberth/internal/recordjson"
)

func fixtureWizardAdmin(ctx context.Context, st WizardState, action string, args, into any) error {
	var result any
	switch action {
	case "upstream-list":
		raw, err := runKubectl(ctx, append(kubectlTarget(&st), "upstream", "list", "--check")...)
		if err != nil {
			return err
		}
		rows := []map[string]any{}
		for _, row := range parseUpstreamCheck(raw) {
			rows = append(rows, map[string]any{"upstream": map[string]string{"name": row.name, "base_url": row.url}, "auth": row.auth})
		}
		result = map[string]any{"upstreams": rows}
	case "upstream-add":
		_, err := runKubectl(ctx, append(kubectlTarget(&st), "upstream", "add", st.ForgeType, "ssh://git@"+forgeUpstreamURL(st.ForgeType, st.ForgeOrg))...)
		return err
	case "public-identity":
		raw, err := runKubectl(ctx, append(kubectlTarget(&st), "admin", "public-identity", "upstream-public-key", "id_ed25519.pub")...)
		if err != nil {
			return err
		}
		result = map[string]string{"content": strings.TrimSpace(string(raw))}
	}
	if into == nil {
		return nil
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return recordjson.Unmarshal(raw, into)
}
