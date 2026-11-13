package installer

import (
	"bytes"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
)

func TestInstallUpstreamHostNames(t *testing.T) {
	for _, tc := range []struct{ url, name string }{
		{"ssh://git@192.0.2.10/repos", "192-0-2-10"},
		{"ssh://git@192.0.2.10:2222/repos", "192-0-2-10"},
		{"ssh://git@[2001:db8::10]/repos", "2001-0db8-0000-0000-0000-0000-0000-0010"},
		{"ssh://git@[2001:db8::10]:2222/repos", "2001-0db8-0000-0000-0000-0000-0000-0010"},
		{"ssh://git@[2001:db8:1234:5678:abcd:ef01:2345:6789]/repos", "2001-0db8-1234-5678-abcd-ef01-2345-6789"},
		{"ssh://git@[2001:db8:10::]/repos", "2001-0db8-0010-0000-0000-0000-0000-0000"},
		{"ssh://git@forge.example.test/repos", "forge"},
		{"ssh://git@forge.example.test:2222/repos", "forge"},
		{"github.com/repos", "github"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			name, _, err := upstreamFromInput(tc.url)
			if err != nil || name != tc.name || gitcache.ValidateUpstreamName(name) != nil {
				t.Fatal(name, err)
			}
			if also := alsoUpstreamName(tc.url, map[string]bool{}, map[string]bool{}); also != tc.name {
				t.Fatal(also)
			}
			cfg := Config{ForgeURL: tc.url}
			review := Review(cfg)
			var output bytes.Buffer
			PrintReview(&output, cfg)
			if review.UpstreamName != tc.name || !strings.Contains(output.String(), "Upstream name: "+tc.name) || !strings.Contains(output.String(), "--upstream-name") {
				t.Fatal(output.String(), review)
			}
		})
	}
}

func TestInstallUpstreamNameOverride(t *testing.T) {
	cfg := Config{ForgeURL: "ssh://git@192.0.2.10/repos", UpstreamName: "project-forge"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	name, _, err := cfg.primaryUpstream()
	if err != nil || name != "project-forge" || Review(cfg).UpstreamName != name {
		t.Fatal(name, err)
	}
	var output bytes.Buffer
	deps := Deps{Output: &output}
	_, name, ok, err := promptUpstream(t.Context(), cfg, deps, newTableWriter(&output, false))
	if err != nil || !ok || name != "project-forge" {
		t.Fatal(name, err)
	}
	for _, name := range []string{"-bad", "metrics", "bad/name", "name.git"} {
		cfg.UpstreamName = name
		if cfg.Validate() == nil {
			t.Fatalf("invalid override accepted: %q", name)
		}
	}
}
