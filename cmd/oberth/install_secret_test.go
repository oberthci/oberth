package main

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestSecretPutGrantsBeforePrivateWriteAndStopsOnRefusal(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "approved", true: "refused"}[refused], func(t *testing.T) {
			assertSecretPutGrantSequence(t, refused)
		})
	}
}

func assertSecretPutGrantSequence(t *testing.T, refused bool) {
	t.Helper()
	bindings := fake.Bindings()
	granted, writes := false, 0
	invalid := publicerr.New(publicerr.InvalidInput, "Test request differs.", "Check the test request.", nil)
	tuple := apicontract.AccessAllowArgs{Repo: "origin/team/widget", Workflow: ".oberth/build.yaml", Step: "build", Secret: "registry/key"}
	bindings.AccessAllow.Handler = apicontract.HandlerFunc[apicontract.AccessAllowArgs, apicontract.Grant](func(ctx context.Context, _ apicontract.Caller, q apicontract.AccessAllowArgs) (apicontract.Call[apicontract.Grant], error) {
		if _, ok := apicontract.SecretValue(ctx); ok || q != tuple {
			t.Error("grant received private bytes or changed authority")
			return nil, invalid
		}
		if refused {
			return nil, publicerr.New(publicerr.Forbidden, "Grant refused.", "Review the requested access.", nil)
		}
		return func(context.Context) (apicontract.Grant, error) {
			granted = true
			return apicontract.Grant{}, nil
		}, nil
	})
	expected := apicontract.SecretPutArgs{Repo: tuple.Repo, Workflow: tuple.Workflow, Step: tuple.Step, Secret: tuple.Secret}
	bindings.SecretPut.Handler = apicontract.HandlerFunc[apicontract.SecretPutArgs, apicontract.Receipt](func(ctx context.Context, _ apicontract.Caller, q apicontract.SecretPutArgs) (apicontract.Call[apicontract.Receipt], error) {
		value, ok := apicontract.SecretValue(ctx)
		if !granted || !ok || string(value) != "private-fixture" || q != expected {
			t.Error("write preceded grant or changed private request")
			return nil, invalid
		}
		return func(context.Context) (apicontract.Receipt, error) {
			writes++
			return apicontract.Receipt{ID: q.Secret, State: "stored"}, nil
		}, nil
	})
	app, out, diagnostic := fixture(t, bindings)
	app.in = strings.NewReader("private-fixture")
	code := app.run(t.Context(), []string{"secret", "put", tuple.Secret, "--repo", tuple.Repo, "--file", tuple.Workflow, "--step", tuple.Step})
	if (code == 0) == refused || writes != map[bool]int{false: 1, true: 0}[refused] {
		t.Fatal("grant/write result differs", code, writes, diagnostic.String())
	}
	assertSecretPutOutput(t, out.String(), diagnostic.String(), map[bool]string{false: "stored", true: "grant-refused"}[refused])
}

func TestSecretPutReportsGrantWhenStoreRefuses(t *testing.T) {
	bindings := fake.Bindings()
	bindings.SecretPut.Handler = &fake.Handler[apicontract.SecretPutArgs, apicontract.Receipt]{Error: publicerr.New(publicerr.Unavailable, "Storage refused.", "Unseal the store, then retry.", nil)}
	app, out, diagnostic := fixture(t, bindings)
	app.in = strings.NewReader("private-fixture")
	code := app.run(t.Context(), []string{"secret", "put", "registry/key", "--repo", "origin/team/widget", "--file", ".oberth/build.yaml", "--step", "build"})
	if code == 0 {
		t.Fatal("failed storage reported success")
	}
	assertSecretPutOutput(t, out.String(), diagnostic.String(), "store-refused")
}

func assertSecretPutOutput(t *testing.T, out, diagnostic, outcome string) {
	t.Helper()
	expected := map[string]struct {
		lines  []string
		reason string
	}{
		"stored":        {[]string{"Secret access granted.\n", "Secret stored for the approved step.\n"}, ""},
		"grant-refused": {[]string{"Secret access not granted; secret not stored.\n"}, "Grant refused."},
		"store-refused": {[]string{"Secret access granted.\n", "Secret not stored.\n"}, "Storage refused."},
	}[outcome]
	offset := 0
	for _, line := range expected.lines {
		index := strings.Index(out[offset:], line)
		if index < 0 {
			t.Fatal("grant/store outcome missing or out of order", out, diagnostic)
		}
		offset += index + len(line)
	}
	if expected.reason != "" && !strings.Contains(diagnostic, expected.reason) {
		t.Fatal("refusal reason missing", diagnostic)
	}
	if strings.Contains(out+diagnostic, "private-fixture") {
		t.Fatal("secret value entered public output")
	}
}
