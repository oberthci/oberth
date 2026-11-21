package client

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSavingOneByteCredentialSurvivesCleanup(t *testing.T) {
	for _, secret := range []string{"x", "xy"} {
		t.Run(secret, func(t *testing.T) {
			assertSavedCredentialSurvivesCleanup(t, secret)
		})
	}
}

func assertSavedCredentialSurvivesCleanup(t *testing.T, secret string) {
	t.Helper()
	path := privateFile(t, "reserved", nil)
	input, err := json.Marshal(map[string]string{"bearer": secret})
	if err != nil {
		t.Fatal(err)
	}
	public, err := SaveCredentials(input, map[string]string{"bearer": path})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err = json.Unmarshal(public, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["bearer"]; exists || fields["bearer_file"] != path {
		t.Fatal("private credential escaped its saved-file result")
	}
	data, err := ReadPrivate(path)
	if err != nil || string(data) != secret {
		t.Fatal("saved nonempty credential was removed or changed", err)
	}
	CleanupCredentials(map[string]string{"bearer": path})
	if _, err = os.Stat(path); err != nil {
		t.Fatal("repeat cleanup removed saved credential", err)
	}
}
