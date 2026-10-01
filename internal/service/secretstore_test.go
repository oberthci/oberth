package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
)

func TestSecretStoreVerifyAuthorizationAndInputs(t *testing.T) {
	t.Parallel()
	fixture := newControlFixture(t)
	calls := 0
	var received api.SecretStoreVerifyRequest
	control, err := NewAPI(APIConfig{Runs: fixture.store, SecretStoreVerifier: func(ctx context.Context, request api.SecretStoreVerifyRequest) (api.SecretStoreVerifyResponse, error) {
		calls++
		received = request
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 45*time.Second {
			t.Fatal("verification callback lacks the bounded default deadline")
		}
		return api.SecretStoreVerifyResponse{Verified: true, Output: "verified"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	actor := api.Actor{Identity: "agent@host"}
	if _, err := control.CallTool(context.Background(), actor, "secretstore_verify", json.RawMessage(`{"address":"https://attacker.invalid"}`)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin must be refused before parsing or I/O: %v", err)
	}
	if calls != 0 {
		t.Fatal("non-admin reached the verifier")
	}
	actor.Admin = true
	for _, input := range []string{
		`{"address":"https://attacker.invalid"}`, `{"role":"root"}`, `{"ca_cert":"/tmp/ca"}`,
		`{"sa_token":"/tmp/token"}`, `{"insecure_http":true}`, `{"kv_mount":"sys"}`,
		`{"timeout":-1}`, `{"timeout":121}`, `{"tier":"admin"}`,
		`{"repo":"upstream/org/repo"}`, `{"release_tier":true,"repo":"upstream//repo"}`,
		`{"release_tier":true,"repo":"upstream/org/.."}`, `{"release_tier":true,"repo":"bare"}`,
		`{"tier":"ci","release_tier":true}`, `{"release_tier":true,"keys":true}`,
		`{"release_tier":true,"expect":["secret/key"]}`, `{"expect":["bad"]}`, `{"expect":["secret/key,"]}`,
		`{"paths":["sys/policy"]}`, `{"paths":["oberth/data/../secret"]}`,
		`{"paths":["oberth/upstream/org/repo/secret/extra"]}`,
		`{"paths":[` + strings.TrimSuffix(strings.Repeat(`"oberth/data/x",`, 33), ",") + `]}`,
	} {
		if _, err := control.CallTool(context.Background(), actor, "secretstore_verify", json.RawMessage(input)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("input %s: got %v, want invalid input", input, err)
		}
	}
	if calls != 0 {
		t.Fatal("invalid request reached the verifier")
	}
	result, err := control.CallTool(context.Background(), actor, "secretstore_verify", json.RawMessage(`{"release_tier":true,"repo":"github/org/repo","paths":["oberth/upstream/org/repo/signing"]}`))
	if err != nil || !result.(api.SecretStoreVerifyResponse).Verified || calls != 1 {
		t.Fatalf("admin verify: result=%v calls=%d err=%v", result, calls, err)
	}
	if received.Timeout != 45 || received.Tier != "release" || received.Repo != "github/org/repo" {
		t.Fatalf("request defaults/identity lost: %+v", received)
	}
}

func TestSecretStoreVerifyHonorsCancellation(t *testing.T) {
	t.Parallel()
	fixture := newControlFixture(t)
	control, err := NewAPI(APIConfig{Runs: fixture.store, SecretStoreVerifier: func(ctx context.Context, _ api.SecretStoreVerifyRequest) (api.SecretStoreVerifyResponse, error) {
		<-ctx.Done()
		return api.SecretStoreVerifyResponse{}, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = control.CallTool(ctx, api.Actor{Identity: "admin@host", Admin: true}, "secretstore_verify", json.RawMessage(`{"timeout":120}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent deadline was not propagated: %v", err)
	}
}
