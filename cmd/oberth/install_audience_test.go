//go:build linux

// This test exercises admission through the Linux server and Git receive path.

package main

import (
	"bytes"
	"encoding/pem"
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/nativebinding"
)

func TestStoreSetupAndAdmissionUseOneAudience(t *testing.T) {
	t.Parallel()
	binary := composeBinary(t)
	for _, audience := range []string{"first-installation", "second-installation"} {
		f := composeFixture(t, binary)
		backend := f.secretBackend(t)
		f.config.SecretStore.Audience = audience
		f.prepareCommit(t)
		f.prepareAdministrator(t)
		cancel, done := f.run(t)
		f.checkStarting(t, done)
		f.waitStarted(t, done)
		certificate, err := os.ReadFile(f.config.Files.SecretStoreCA)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(certificate)
		request := storeOperatorRequest(t, f.config, block.Bytes)
		if err := runStoreOperator(t.Context(), request, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		admission := f.owners.native.Hooks(nativebinding.Preparation{})
		for _, role := range []string{f.config.SecretStore.WriterRole, "oberth-step"} {
			resource := backend.Resources["/auth/"+f.config.SecretStore.AuthMount+"/role/"+role]
			if admission.LoginAudience != audience || resource["audience"] != admission.LoginAudience {
				t.Fatalf("store/admission audiences differ: %v %q", resource, admission.LoginAudience)
			}
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
