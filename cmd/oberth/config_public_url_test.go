package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestConfigRejectsUnicodePublicHostWithPunycodeRecovery(t *testing.T) {
	for _, pair := range [][2]string{
		{"https://bücher.example", "https://xn--bcher-kva.example"},
		{"https://bücher.example:34443/mcp", "https://xn--bcher-kva.example:34443/mcp"},
		{"https://\u212Aube.local", "https://kube.local"},
	} {
		data := configCase(t, func(c map[string]any) { c["public"].(map[string]any)["url"] = pair[0] })
		path := filepath.Join(t.TempDir(), "server.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readConfig(path)
		if err == nil {
			t.Fatal("Unicode public host accepted")
		}
		problem := publicerr.Classify(err)
		if !strings.Contains(problem.Detail, path) || !strings.Contains(problem.Remedy(), "punycode") || publicerr.CheckMessage(problem.Reason(), 80) != nil || publicerr.CheckMessage(problem.Remedy(), 120) != nil {
			t.Fatal("host refusal lacks bounded recovery", err)
		}
		app, _, diagnostic := fixture(t, fake.Bindings())
		app.failed(err)
		if !strings.Contains(diagnostic.String(), "public.url:\n"+pair[1]+"\n") {
			t.Fatal("punycode recovery is hidden", diagnostic.String())
		}
		data = configCase(t, func(c map[string]any) { c["public"].(map[string]any)["url"] = pair[1] })
		if _, err = decodeConfig(data); err != nil {
			t.Fatal("suggested punycode URL was refused", err)
		}
	}
}
