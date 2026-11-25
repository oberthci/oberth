package main

import "testing"

func TestInstallAndUpgradeAcceptAnExplicitHTTPSServicePort(t *testing.T) {
	for _, name := range []string{"install", "upgrade"} {
		t.Run(name, func(t *testing.T) {
			flags := installationFlags(name)
			if err := flags.Parse([]string{"--service-port=443"}); err != nil {
				t.Fatal(err)
			}
			request := installationRequest{name: name, flags: flags}
			if request.choices().HTTPSServicePort != 443 {
				t.Fatal("explicit HTTPS Service port did not reach installation choices")
			}
			unset := installationRequest{name: name, flags: installationFlags(name)}
			if unset.choices().HTTPSServicePort != 0 {
				t.Fatal("an omitted flag would overwrite the saved Service port")
			}
		})
	}
}
