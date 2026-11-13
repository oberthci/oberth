package installer

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestReviewedUpstreamNameReachesRegistration(t *testing.T) {
	for _, test := range []struct{ url, override, name string }{
		{"ssh://git@192.0.2.10:2222/repos", "", "192-0-2-10"},
		{"ssh://git@[2001:db8::10]:2222/repos", "", "2001-0db8-0000-0000-0000-0000-0000-0010"},
		{"ssh://git@[2001:db8::10]:2222/repos", "project-forge", "project-forge"},
		{"ssh://git@forge.example.test:2222/repos", "", "forge"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := &fakeOberthHost{t: t}
			var output bytes.Buffer
			deps := onboardingDeps(t, pod, &output, strings.NewReader(""), false)
			var registered InstallUpstream
			deps.upstreamResult = func(value InstallUpstream) { registered = value }
			cfg := Config{ForgeURL: test.url, UpstreamName: test.override, ExpectedHostFingerprint: "SHA256:" + strings.Repeat("a", 43), ClientAccess: "none", SSHPublicKeyPath: "-", UplinkIdentity: "admin@workstation", uplinkOnboarded: true}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			if Review(cfg).UpstreamName != test.name {
				t.Fatal("wrong reviewed name")
			}
			table := newTableWriter(&output, false)
			table.WriteHeader()
			table.WriteFooter()
			if err := runOnboarding(context.Background(), cfg, deps, table, &heldCredentials{}); err != nil {
				t.Fatal(err)
			}
			if pod.addCalls != 1 || registered.Name != test.name || registered.URL != test.url {
				t.Fatal("registration result differs from Review", registered)
			}
			args := pod.addArgv[0]
			if len(args) < 2 || args[len(args)-2] != test.name || args[len(args)-1] != test.url {
				t.Fatal("actual upstream add argv differs from Review", args)
			}
		})
	}
}
