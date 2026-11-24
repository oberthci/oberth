package main

import (
	"encoding/json"
	"testing"
)

func TestInstallExplicitEgressSurvivesChoices(t *testing.T) {
	for _, command := range []string{"install", "upgrade"} {
		flags := installationFlags(command)
		if err := flags.Parse([]string{"--egress-allow", `[{"cidr":"192.0.2.10/32","port":22,"protocol":"UDP"}]`, "--egress-internet=false", "--no-egress"}); err != nil {
			t.Fatal(err)
		}
		choices := (installationRequest{name: command, flags: flags}).choices()
		raw, _ := json.Marshal(choices.Values)
		if choices.Egress == nil || *choices.Egress || string(raw) != `{"policies":{"allow":[{"cidr":"192.0.2.10/32","port":22,"protocol":"UDP"}],"internet":false}}` {
			t.Fatal("explicit destinations or empty reset lost", string(raw))
		}
	}
	for _, raw := range []string{"null", `[{}]`, `[{"cidr":"host.test","port":443}]`, `[{"cidr":"192.0.2.1/24","port":22}]`, `[{"cidr":"192.0.2.1/32","port":0}]`, `[{"cidr":"192.0.2.1/32","port":443,"protocol":"SCTP"}]`, `[{"cidr":"0.0.0.0/0","port":443,"protocol":"TCP","except":["10.0.0.0/8"]}]`} {
		if err := new(egressDestinations).Set(raw); err == nil {
			t.Fatal("invalid network allowance accepted", raw)
		}
	}
}
