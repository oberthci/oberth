package setuptui

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/oberthci/oberth/internal/api"
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
		for _, row := range fixtureUpstreamCheck(raw) {
			rows = append(rows, map[string]any{"upstream": map[string]string{"name": row.name, "base_url": row.url}, "auth": row.auth})
		}
		result = map[string]any{"upstreams": rows}
	case "upstream-add":
		raw, err := json.Marshal(args)
		if err != nil {
			return err
		}
		var request api.UpstreamAddRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return err
		}
		words := []string{"upstream", "add", "--yes"}
		if request.NoWait {
			words = append(words, "--no-wait")
		}
		if request.ExpectedHostFingerprint != "" {
			words = append(words, "--expected-host-fingerprint", request.ExpectedHostFingerprint)
		}
		words = append(words, request.Name, request.BaseURL)
		_, err = runKubectl(ctx, append(kubectlTarget(&st), words...)...)
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

func fixtureUpstreamCheck(out []byte) []upstreamCheck {
	authAt := -1
	header := false
	var rows []upstreamCheck
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !header {
			if fields[0] == "NAME" {
				header = true
				authAt = strings.Index(line, "AUTH")
			}
			continue
		}
		row := upstreamCheck{name: fields[0]}
		for _, f := range fields[1:] {
			if strings.Contains(f, "://") {
				row.url = f
				break
			}
		}
		if authAt >= 0 && len(line) > authAt {
			row.auth = strings.TrimSpace(line[authAt:])
		}
		rows = append(rows, row)
	}
	return rows
}
