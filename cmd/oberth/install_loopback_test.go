package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestDerivedLoopbackRequiresReachableURL(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "[::1]", "[::1%25lo]", "localhost", "LOCALHOST."} {
		t.Run(host, func(t *testing.T) {
			s, release := addressSession(t, host)
			var out bytes.Buffer
			plan, err := s.prepareReachableInstallation(t.Context(), release, &out)
			if plan != nil {
				defer plan.Close()
			}
			if err == nil || plan != nil || out.Len() != 0 || !strings.Contains(err.Error(), "--url") {
				t.Fatal("loopback default accepted", plan, err, out.String())
			}
			s.interactivePrompt = true
			s.input = bufio.NewScanner(strings.NewReader("https://ci.example:30443\n\napproval\n"))
			plan, err = s.prepareReachableInstallation(t.Context(), release, &out)
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Close()
			if !strings.Contains(strings.Join(plan.Review(), "\n"), "https://ci.example:30443") || !s.input.Scan() || s.input.Text() != "approval" {
				t.Fatal("reachable URL or approval lost", out.String())
			}
		})
	}
}
