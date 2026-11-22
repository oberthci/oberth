package main

import (
	"bytes"
	"testing"
)

func TestFinalizeReportsTheLatestDestinationAndVersion(t *testing.T) {
	for _, tc := range []struct{ current, latest, suffix string }{
		{"", "v2.0.0", "is v2.0.0."},
		{"v2.0.0", "v2.0.0", "is v2.0.0."},
		{"v1.0.0", "v2.0.0", "is v2.0.0."},
		{"v3.0.0", "v3.0.0", "stays v3.0.0; v2.0.0 was not made latest."},
	} {
		t.Run(tc.current+" "+tc.suffix, func(t *testing.T) {
			p, fake := githubFixture(t)
			for _, tag := range []string{"v1.0.0", "v2.0.0", "v3.0.0"} {
				fake.add(tag, false)
			}
			fake.latest = tc.current
			var output bytes.Buffer
			if err := p.finish(t.Context(), "v2.0.0", &output); err != nil {
				t.Fatal("successful finalization returned failure", err)
			}
			if output.String() != "Latest at https://github.com/oberthci/oberth/releases/latest "+tc.suffix+"\n" {
				t.Fatal("closing line is missing or ambiguous", output.String())
			}
			if fake.latest != tc.latest {
				t.Fatal("closing line differs from latest", fake.latest)
			}
		})
	}
}
