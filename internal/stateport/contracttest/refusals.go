package contracttest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func invalidOptions(t *testing.T, create factory) {
	t.Helper()
	for _, tc := range []struct {
		name   string
		change func(*stateport.Options)
		code   publicerr.Code
	}{
		{"empty_protocol", func(o *stateport.Options) { o.Protocol = stateport.Protocol{} }, publicerr.InvalidInput},
		{"missing_schema", func(o *stateport.Options) { o.Schema = 0 }, publicerr.InvalidInput},
		{"negative_limit", func(o *stateport.Options) { o.MaxBytes = -1 }, publicerr.InvalidInput},
		{"excessive_limit", func(o *stateport.Options) { o.MaxBytes = stateport.HardMaxBytes + 1 }, publicerr.InvalidInput},
		{"changed_schema", func(o *stateport.Options) { o.Schema++ }, publicerr.SchemaRefused},
		{"changed_domain", func(o *stateport.Options) { o.Protocol.AuditDomain += "-other" }, publicerr.SchemaRefused},
		{"changed_identity", func(o *stateport.Options) { o.Protocol.SchemaIdentity += "-other" }, publicerr.SchemaRefused},
		{"measured_size_limit", func(o *stateport.Options) { o.MaxBytes = 1 }, publicerr.InvalidInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, operation := range []string{"snapshot", "inspect", "restore"} {
				t.Run(operation, func(t *testing.T) {
					f := prepare(t, create, 0)
					body, _ := f.snapshot(t)
					altered := f.Options
					tc.change(&altered)
					checkInvalid(t, f, body, invalidCase{altered, operation, tc.code})
				})
			}
		})
	}
}

type invalidCase struct {
	options   stateport.Options
	operation string
	code      publicerr.Code
}

func checkInvalid(t *testing.T, f Fixture, body []byte, tc invalidCase) {
	t.Helper()
	var output bytes.Buffer
	request := f.restoreRequest(t)
	var err error
	switch tc.operation {
	case "snapshot":
		_, err = f.Port.Snapshot(t.Context(), tc.options, &output)
	case "inspect":
		_, err = f.Port.Inspect(t.Context(), tc.options, bytes.NewReader(body))
	case "restore":
		bad := request
		bad.Options = tc.options
		_, err = f.Port.Restore(t.Context(), bad, bytes.NewReader(body))
	}
	assertCode(t, err, tc.code)
	if output.Len() != 0 {
		t.Fatal("refusal emitted unverified output")
	}
	f.assertDestination(t, request.Destination, false)
	if _, err := f.Port.Restore(t.Context(), request, bytes.NewReader(body)); err != nil {
		t.Fatal("refused operation changed destination", err)
	}
	f.assertDestination(t, request.Destination, true)
}

func missingInputs(t *testing.T, f Fixture) {
	t.Helper()
	_, err := f.Port.Snapshot(t.Context(), f.Options, nil)
	assertCode(t, err, publicerr.InvalidInput)
	_, err = f.Port.Inspect(t.Context(), f.Options, nil)
	assertCode(t, err, publicerr.InvalidInput)
	request := f.restoreRequest(t)
	request.Destination = ""
	_, err = f.Port.Restore(t.Context(), request, bytes.NewReader(nil))
	assertCode(t, err, publicerr.InvalidInput)
}

func streamErrors(t *testing.T, f Fixture) {
	t.Helper()
	cause := errors.New("stream fixture failed")
	_, err := f.Port.Snapshot(t.Context(), f.Options, failedWriter{cause})
	if !errors.Is(err, cause) {
		t.Fatalf("write cause lost: %v", err)
	}
	_, err = f.Port.Inspect(t.Context(), f.Options, failedReader{cause})
	if !errors.Is(err, cause) {
		t.Fatalf("read cause lost: %v", err)
	}
}

func canceledStreams(t *testing.T, f Fixture) {
	t.Helper()
	body, _ := f.snapshot(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := f.Port.Snapshot(ctx, f.Options, cancelWriter{cancel})
	assertCanceled(t, err)
	ctx, cancelRead := context.WithCancel(t.Context())
	defer cancelRead()
	request := f.restoreRequest(t)
	_, err = f.Port.Restore(ctx, request, cancelReader{bytes.NewReader(body), cancelRead})
	assertCanceled(t, err)
	f.assertDestination(t, request.Destination, false)
	if _, err := f.Port.Restore(t.Context(), request, bytes.NewReader(body)); err != nil {
		t.Fatal("canceled restore changed destination", err)
	}
	f.assertDestination(t, request.Destination, true)
}

type failedWriter struct{ err error }

func (w failedWriter) Write([]byte) (int, error) { return 0, w.err }

type failedReader struct{ err error }

func (r failedReader) Read([]byte) (int, error) { return 0, r.err }

type cancelWriter struct{ cancel context.CancelFunc }

func (w cancelWriter) Write(body []byte) (int, error) { w.cancel(); return len(body), nil }

type cancelReader struct {
	io.Reader
	cancel context.CancelFunc
}

func (r cancelReader) Read(body []byte) (int, error) {
	n, err := r.Reader.Read(body)
	r.cancel()
	return n, err
}
