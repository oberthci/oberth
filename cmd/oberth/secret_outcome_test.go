package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestSecretMutationLostRepliesReportUnknownWithoutReplay(t *testing.T) {
	for _, name := range []string{"grant-eof", "grant-generic", "grant-json", "grant-status", "grant-redirect", "store-eof", "store-generic", "store-json", "store-read", "store-status", "store-redirect"} {
		t.Run(name, func(t *testing.T) { secretOutcomeCase(t, name) })
	}
}

func secretOutcomeCase(t *testing.T, name string) {
	t.Helper()
	stage, failure, _ := strings.Cut(name, "-")
	var grants, writes, redirected atomic.Int32
	bindings := fake.Bindings()
	bindings.AccessAllow.Handler = &fake.Handler[apicontract.AccessAllowArgs, apicontract.Grant]{Run: func(context.Context) (apicontract.Grant, error) { grants.Add(1); return apicontract.Grant{}, nil }}
	bindings.SecretPut.Handler = &fake.Handler[apicontract.SecretPutArgs, apicontract.Receipt]{Run: func(context.Context) (apicontract.Receipt, error) {
		writes.Add(1)
		return apicontract.Receipt{ID: "registry/key", State: "stored"}, nil
	}}
	app, out, diagnostic := fixture(t, bindings)
	app.config.Transport = mutationReplyLoss(app.config.Transport, stage, failure, &redirected)
	app.in = strings.NewReader("private-outcome-canary")
	code := app.run(t.Context(), []string{"secret", "put", "registry/key", "--repo", "origin/team/widget", "--file", ".oberth/build.yaml", "--step", "build"})
	wantWrites := int32(1)
	if stage == "grant" {
		wantWrites = 0
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect target received a request")
	}
	if code != 1 || grants.Load() != 1 || writes.Load() != wantWrites {
		t.Fatal("mutation was replayed or write followed an uncertain grant", code, grants.Load(), writes.Load())
	}
	assertSecretOutcomeText(t, out.String(), diagnostic.String(), stage)
}

func assertSecretOutcomeText(t *testing.T, output, diagnostic, stage string) {
	t.Helper()
	wantOutput, check := "Secret storage outcome is unknown.", "oberth audit"
	if stage == "grant" {
		wantOutput = "Secret access outcome is unknown; secret was not sent."
		check = "oberth access list"
	}
	if !strings.Contains(output, wantOutput) || strings.Contains(output, "not stored") || strings.Contains(output, "not granted") {
		t.Fatal("uncertain mutation was reported as refused", output)
	}
	if stage == "store" && !strings.Contains(strings.Join(strings.Fields(diagnostic), " "), "putting the same value again is safe") {
		t.Fatal("storage caller omitted safe repeat guidance", diagnostic)
	}
	if !strings.Contains(diagnostic, "outcome is unknown") || !strings.Contains(diagnostic, check) {
		t.Fatal("uncertain outcome has no state check", diagnostic)
	}
	if strings.Contains(output+diagnostic, "private-outcome-canary") {
		t.Fatal("secret value entered output")
	}
}

func lostMutationReply(reply *http.Response, failure string) (*http.Response, error) {
	switch failure {
	case "eof":
		return nil, io.ErrUnexpectedEOF
	case "generic":
		return nil, errors.New("private transport reply loss")
	case "redirect":
		reply.StatusCode = http.StatusTemporaryRedirect
		reply.Header.Set("Location", "https://redirect.oberth.local/never")
		reply.Body = http.NoBody
	case "status":
		reply.StatusCode = http.StatusServiceUnavailable
		reply.Body = io.NopCloser(strings.NewReader("{"))
	case "read":
		reply.Body = io.NopCloser(failedSecretReply{})
	default:
		reply.Body = io.NopCloser(strings.NewReader("{"))
	}
	return reply, nil
}

type failedSecretReply struct{}

func (failedSecretReply) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func mutationReplyLoss(original http.RoundTripper, stage, failure string, redirected *atomic.Int32) http.RoundTripper {
	return testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "redirect.oberth.local" {
			redirected.Add(1)
			return nil, errors.New("redirect target was contacted")
		}
		reply, err := original.RoundTrip(r)
		if err != nil || stage == "store" && r.URL.Path != "/api/secrets/put" {
			return reply, err
		}
		_ = reply.Body.Close()
		return lostMutationReply(reply, failure)
	})
}

func TestSecretStorageGuidanceDoesNotMutateClientError(t *testing.T) {
	remote, err := client.New(client.Config{Options: client.Options{URL: "https://oberth.local"}, Now: fake.NewClock().Now, Transport: testTransport(func(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF })})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	_, err = remote.PutSecret(t.Context(), apicontract.SecretPutArgs{Secret: "registry/key"}, []byte("fixture"))
	var original *publicerr.Error
	if !client.IsOutcomeUnknown(err) || !errors.As(err, &original) {
		t.Fatal("expected unknown client outcome", err)
	}
	before := original.Remedy()
	guidance := secretWriteGuidance(err)
	var updated *publicerr.Error
	if !client.IsOutcomeUnknown(guidance) || !errors.Is(guidance, err) || !errors.As(guidance, &updated) || updated == original || original.Remedy() != before || !strings.Contains(updated.Remedy(), "putting the same value again is safe") || !errors.Is(guidance, io.ErrUnexpectedEOF) {
		t.Fatal("guidance lost the unknown outcome, mutated the client error or discarded its cause", guidance)
	}
}
