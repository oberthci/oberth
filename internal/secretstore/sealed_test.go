package secretstore

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestSealedStoreGivesManualUnsealActionWithoutBody(t *testing.T) {
	client, err := New(Connection{URL: "https://store.oberth.svc", Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"errors"
 "encoding/base64":["private reply"]}`))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Mount: "oberth", AuthMount: "kubernetes", WriterRole: "writer", StepRole: "step", ServerNamespace: "oberth", PipelineNamespace: "oberth-runs", ServerAccount: "oberth", Audience: "store-only"}
	body := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"system:serviceaccount:oberth-runs:step-one"}`))
	_, err = client.Fetch(t.Context(), cfg, []byte("header."+body+".signature"), []string{"value"})
	var refusal *publicerr.Error
	if !errors.As(err, &refusal) || refusal.Code() != publicerr.Unavailable || refusal.Reason() != "Secret store is sealed." || refusal.Remedy() != "An administrator runs oberth unseal, then retry." || strings.Contains(err.Error(), "private reply") {
		t.Fatal("sealed store gave no manual recovery action", err)
	}
}
