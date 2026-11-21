package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareSigningSeparatesDownloadFromOfflineSigning(t *testing.T) {
	for _, tc := range []signingPreparationCase{
		{"", ""},
		{"network", "Signing tool download failed.\nCheck network access, then rerun hack/release.sh prepare-signing.\n"},
		{"digest", "Signing tool digest differs; fetch the pinned signing tool.\n"},
	} {
		t.Run(tc.failure, func(t *testing.T) {
			checkSigningPreparation(t, tc)
		})
	}
}

type signingPreparationCase struct{ failure, want string }

func checkSigningPreparation(t *testing.T, tc signingPreparationCase) {
	t.Helper()

	env := signingEnvironment(t)
	env["PREPARE_FAILURE"], env["RELEASE_ORIGIN"] = tc.failure, ""
	bin := filepath.Dir(env["SIGNER"])
	writeFixtureCommand(t, filepath.Join(bin, "curl"), `if [ "$PREPARE_FAILURE" = network ]; then echo native-error >&2; exit 1; fi
while [ "$#" -gt 0 ]; do
    if [ "$1" = --output ]; then shift; cp "$SIGNER" "$1"; exit; fi
    shift
done
exit 1
`)
	writeFixtureCommand(t, filepath.Join(bin, "sha256sum"), `if [ "$PREPARE_FAILURE" = digest ]; then echo native-error >&2; exit 1; fi
read -r hash file
[ "$hash" = ae1ecd212663f3693ad9edf8b1a183900c9a52d3155ba6e354237f9a0f6463fc ] && [ -s "$file" ]
`)
	want := tc.want
	if tc.failure != "" {
		want += "Detail: " + filepath.Join(env["RELEASE_PRIVATE"], "cosign.error") + "\n"
	}
	output, err := releaseScript(t, "prepare-signing", env)
	if (err == nil) != (tc.failure == "") || string(output) != want {
		t.Fatal("signer preparation lost the whole refusal or leaked tool output", err, string(output))
	}
	if tc.failure != "" {
		cause, err := os.ReadFile(filepath.Join(env["RELEASE_PRIVATE"], "cosign.error"))
		if err != nil || string(cause) != "native-error\n" {
			t.Fatal("named cause file lost the native error", err)
		}
	}
	if tc.failure == "" {
		info, err := os.Stat(filepath.Join(env["RELEASE_PRIVATE"], "cosign"))
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("verified signer is not executable", err)
		}
	}
}
