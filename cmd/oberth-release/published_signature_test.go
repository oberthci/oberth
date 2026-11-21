package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

type signatureCase struct {
	status  int
	body    string
	allowed bool
}

func TestPublicationChecksExistingSignatureBeforeAnyWrite(t *testing.T) {
	for _, tc := range []signatureCase{
		{403, "", true}, {404, "", true}, {200, "signed", true},
		{200, "another signature", false}, {401, "", false}, {500, "", false},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.body), func(t *testing.T) {
			checkSignaturePublication(t, tc)
		})
	}
}

func checkSignaturePublication(t *testing.T, tc signatureCase) {
	t.Helper()
	p, _ := storeFixture(t)
	if err := os.WriteFile(filepath.Join(p.dir, "SHA256SUMS.sigstore.json"), []byte("signed"), 0600); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Path != "/v1.2.3/SHA256SUMS.sigstore.json" || writes.Load() != 0 {
				t.Error("signature check came after a write or read the wrong object")
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
			return
		}
		writes.Add(1)
		w.WriteHeader(201)
	}))
	defer server.Close()
	p.origin, p.client = server.URL, server.Client()
	err := p.publishFiles(t.Context(), "v1.2.3")
	if (err == nil) != tc.allowed || (writes.Load() > 0) != tc.allowed {
		t.Fatal("signature preflight result", err, writes.Load())
	}
	if tc.status == 200 && !tc.allowed && !errors.Is(err, immutable) {
		t.Fatal("different signature lacks immutable-version remedy", err)
	}
}
