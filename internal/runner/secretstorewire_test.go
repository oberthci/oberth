package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func deliveredPayload() SecretStorePayload {
	return SecretStorePayload{
		Version: SecretStorePayloadVersion,
		Secrets: []SecretStorePayloadSecret{
			{Name: "gar-token", Keys: map[string][]byte{"token": []byte("gar-value")}},
			{Name: "r2-token", Keys: map[string][]byte{
				"access": []byte("r2-access"),
				"secret": []byte("r2-secret\nsecond-line"),
			}},
		},
	}
}

func TestSecretStoreDeliveryRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	encoded, err := EncodeSecretStorePayload(deliveredPayload())
	if err != nil {
		t.Fatal(err)
	}
	if err := ReceiveSecretStoreSecrets(bytes.NewReader(encoded), dir); err != nil {
		t.Fatal(err)
	}
	values, err := AwaitSecretStoreSecrets(context.Background(), dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(values)
	want := []string{"gar-value", "r2-access", "r2-secret\nsecond-line"}
	if !slices.Equal(values, want) {
		t.Fatalf("delivered values = %q, want %q", values, want)
	}
	value, err := os.ReadFile(filepath.Join(dir, "r2-token", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != "r2-secret\nsecond-line" {
		t.Fatalf("delivered file = %q", value)
	}
	info, err := os.Stat(filepath.Join(dir, "r2-token", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Fatalf("delivered file mode = %v, want 0400", info.Mode().Perm())
	}
}

func TestSecretStoreDeliveryIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	encoded, err := EncodeSecretStorePayload(deliveredPayload())
	if err != nil {
		t.Fatal(err)
	}
	if err := ReceiveSecretStoreSecrets(bytes.NewReader(encoded), dir); err != nil {
		t.Fatal(err)
	}
	if err := ReceiveSecretStoreSecrets(bytes.NewReader(encoded), dir); err != nil {
		t.Fatalf("redelivery of the identical payload failed: %v", err)
	}
	if _, err := AwaitSecretStoreSecrets(context.Background(), dir, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestAwaitSecretStoreSecretsTimesOutWithoutDelivery(t *testing.T) {
	t.Parallel()
	_, err := AwaitSecretStoreSecrets(context.Background(), t.TempDir(), 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("missing delivery error = %v", err)
	}
}

func TestAwaitSecretStoreSecretsRejectsTamperedValue(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	encoded, err := EncodeSecretStorePayload(deliveredPayload())
	if err != nil {
		t.Fatal(err)
	}
	if err := ReceiveSecretStoreSecrets(bytes.NewReader(encoded), dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "gar-token", "token")
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("tampered"), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := AwaitSecretStoreSecrets(context.Background(), dir, 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "manifest digest") {
		t.Fatalf("tampered value error = %v", err)
	}
}

func TestAwaitSecretStoreSecretsRejectsUnexpectedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	encoded, err := EncodeSecretStorePayload(deliveredPayload())
	if err != nil {
		t.Fatal(err)
	}
	if err := ReceiveSecretStoreSecrets(bytes.NewReader(encoded), dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gar-token", "extra"), []byte("foreign"), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := AwaitSecretStoreSecrets(context.Background(), dir, 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "unexpected file") {
		t.Fatalf("unexpected file error = %v", err)
	}
}

func TestAwaitSecretStoreSecretsWaitsForManifest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	encoded, err := EncodeSecretStorePayload(deliveredPayload())
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan error, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		delivered <- ReceiveSecretStoreSecrets(bytes.NewReader(encoded), dir)
	}()
	values, err := AwaitSecretStoreSecrets(context.Background(), dir, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if deliverErr := <-delivered; deliverErr != nil {
		t.Fatal(deliverErr)
	}
	if len(values) != 3 {
		t.Fatalf("delivered values = %q", values)
	}
}

func TestReceiveSecretStoreSecretsRejectsUnsafePayloads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload SecretStorePayload
		want    string
	}{
		{name: "version", payload: SecretStorePayload{Version: 2, Secrets: []SecretStorePayloadSecret{{Name: "a", Keys: map[string][]byte{"k": []byte("v")}}}}, want: "version"},
		{name: "empty", payload: SecretStorePayload{Version: 1}, want: "no secrets"},
		{name: "traversal name", payload: SecretStorePayload{Version: 1, Secrets: []SecretStorePayloadSecret{{Name: "../escape", Keys: map[string][]byte{"k": []byte("v")}}}}, want: "unsafe"},
		{name: "slash name", payload: SecretStorePayload{Version: 1, Secrets: []SecretStorePayloadSecret{{Name: "a/b", Keys: map[string][]byte{"k": []byte("v")}}}}, want: "unsafe"},
		{name: "dot key", payload: SecretStorePayload{Version: 1, Secrets: []SecretStorePayloadSecret{{Name: "a", Keys: map[string][]byte{".hidden": []byte("v")}}}}, want: "unsafe"},
		{name: "traversal key", payload: SecretStorePayload{Version: 1, Secrets: []SecretStorePayloadSecret{{Name: "a", Keys: map[string][]byte{"..": []byte("v")}}}}, want: "unsafe"},
		{name: "no keys", payload: SecretStorePayload{Version: 1, Secrets: []SecretStorePayloadSecret{{Name: "a"}}}, want: "no keys"},
		{name: "empty value", payload: SecretStorePayload{Version: 1, Secrets: []SecretStorePayloadSecret{{Name: "a", Keys: map[string][]byte{"k": nil}}}}, want: "empty value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(test.payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := ReceiveSecretStoreSecrets(bytes.NewReader(encoded), t.TempDir()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestReceiveSecretStoreSecretsRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	body := `{"version":1,"secrets":[{"name":"a","keys":{"k":"dg=="},"extra":true}]}`
	if err := ReceiveSecretStoreSecrets(strings.NewReader(body), t.TempDir()); err == nil || !strings.Contains(err.Error(), "decode secret store payload") {
		t.Fatalf("unknown field error = %v", err)
	}
}
