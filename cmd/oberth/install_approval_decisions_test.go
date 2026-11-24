package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/installer"
)

func TestInstallationAutomaticApprovalObservesCancellation(t *testing.T) {
	s, release := addressSession(t, "cluster.oberth.ci")
	plan, err := s.engine.Prepare(t.Context(), release, s.choices)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	if err := s.request.flags.Set("yes", "true"); err != nil {
		t.Fatal(err)
	}
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if canceled {
			cancel()
		}
		var out bytes.Buffer
		approved, err := installationApproval(ctx, installationReview{request: s.request, plan: plan}, strings.NewReader(""), &out)
		cancel()
		if canceled {
			if !errors.Is(err, context.Canceled) {
				t.Fatal("canceled automatic approval succeeded", approved, err)
			}
		} else if err != nil || approved != plan.Digest() {
			t.Fatal("automatic approval lost the reviewed digest", approved, err)
		}
	}
}

func TestUnsealDispatchPropagatesOwnerRefusal(t *testing.T) {
	credentials, err := installer.OpenConfigDir(filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer credentials.Close()
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.install = func(ctx context.Context, r installationRequest, in io.Reader, out io.Writer) error {
		s := installationSession{request: r, engine: &installer.Engine{Credentials: credentials}}
		err := s.dispatch(ctx, in, out)
		var refusal *installer.Problem
		if !errors.As(err, &refusal) || refusal.Code != "custody" {
			t.Fatal("unseal did not reach its owner", err)
		}
		return err
	}
	if code := app.run(t.Context(), []string{"unseal"}); code != 1 || out.Len() != 0 || diagnostic.Len() == 0 {
		t.Fatal("unseal refusal reported success", code, out.String(), diagnostic.String())
	}
}
