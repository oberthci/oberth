package secretstore_test

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/secretstore/fake"
)

func config() secretstore.Config {
	return secretstore.Config{Mount: "oberth", AuthMount: "kubernetes", WriterRole: "oberth-writer", StepRole: "oberth-step", ServerNamespace: "oberth", PipelineNamespace: "oberth-runs", ServerAccount: "oberth", Audience: "oberth-store"}
}
func TestStaticSetup(t *testing.T) {
	backend := &fake.Store{}
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Setup(t.Context(), []byte("temporary"), config()); err != nil {
		t.Fatal(err, errors.Unwrap(err), backend.Resources["/auth/kubernetes/config"])
	}
	if err = client.Check(t.Context(), []byte("temporary"), config()); err != nil {
		t.Fatal(err)
	}
}
