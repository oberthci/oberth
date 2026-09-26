package secretstore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestServiceAccountTokenSourceClearsOwnedBuffers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		body      string
		sourceErr error
		wantErr   bool
	}{
		{name: "success", body: "  " + testServiceJWT + "\n"},
		{name: "error with buffer", body: "partial-credential", sourceErr: errors.New("token request failed"), wantErr: true},
		{name: "empty", wantErr: true},
		{name: "whitespace", body: " \n\t", wantErr: true},
		{name: "oversize", body: strings.Repeat("x", maxTokenBytes+1), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owned := []byte(tc.body)
			client := &Client{tokenPath: "/must-not-be-read", tokenSource: func(context.Context) ([]byte, error) { return owned, tc.sourceErr }}
			token, err := client.serviceAccountToken(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("token source error = %v", err)
			}
			if !bytes.Equal(owned, make([]byte, len(owned))) {
				t.Fatal("owned token-source buffer was not cleared")
			}
			if !tc.wantErr && string(token) != testServiceJWT {
				t.Fatal("successful provider token was not independently copied")
			}
			clear(token)
		})
	}
}

func TestFetchKVUsesMemoryTokenAndDeduplicatesPaths(t *testing.T) {
	t.Parallel()
	mock := newMockVault(t)
	config := mock.config()
	owned := []byte(testServiceJWT)
	config.ServiceAccountTokenPath = ""
	config.ServiceAccountTokenSource = func(context.Context) ([]byte, error) { return owned, nil }
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.FetchKV(context.Background(), []string{"legacy/gar", "legacy/gar"})
	if err != nil {
		t.Fatal(err)
	}
	defer clearKVValues(result["legacy/gar"])
	if string(result["legacy/gar"]["token"]) != "gar-token-value" || mock.reads.Load() != 1 || mock.revokes.Load() != 1 {
		t.Fatal("memory-backed login did not perform one read and revoke")
	}
	if !bytes.Equal(owned, make([]byte, len(owned))) {
		t.Fatal("login retained the provider buffer")
	}
	config.ServiceAccountTokenPath = "/ambiguous-identity"
	if _, err := New(config); err == nil {
		t.Fatal("client accepted both a token provider and token file")
	}
}

func TestFetchKVPathsClearsPartialFailureBuffers(t *testing.T) {
	t.Parallel()
	for _, failRead := range []bool{false, true} {
		t.Run(map[bool]string{true: "later read fails", false: "aggregate size exceeded"}[failRead], func(t *testing.T) {
			first := []byte("first-sensitive-value")
			second := []byte("second-sensitive-value")
			reads := 0
			result, err := fetchKVPaths([]string{"first", "second"}, func(path string) (map[string][]byte, int, error) {
				reads++
				if path == "first" {
					return map[string][]byte{"value": first}, maxSecretTotalBytes, nil
				}
				var readErr error
				if failRead {
					readErr = errors.New("second path unavailable")
				}
				return map[string][]byte{"value": second}, 1, readErr
			})
			if err == nil || result != nil || reads != 2 {
				t.Fatalf("failed collection returned values: result=%v reads=%d error=%v", result, reads, err)
			}
			for _, value := range [][]byte{first, second} {
				if !bytes.Equal(value, make([]byte, len(value))) {
					t.Fatal("failed collection abandoned an owned secret buffer")
				}
			}
		})
	}
}
