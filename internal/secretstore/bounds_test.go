package secretstore_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/secretstore/fake"
)

func boundedWriter(t *testing.T) (secretstore.Writer, *fake.Store) {
	t.Helper()
	backend := &fake.Store{}
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
	if err != nil {
		t.Fatal(err)
	}
	return secretstore.Writer{Client: client, Config: config(), Token: func(context.Context) ([]byte, error) { return []byte("projected-token"), nil }}, backend
}

func TestWriterValuePathAndTokenBounds(t *testing.T) {
	for _, kind := range []string{"value", "path", "token"} {
		for _, extra := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/%d", kind, extra), func(t *testing.T) {
				writer, backend := boundedWriter(t)
				value, path := []byte("value"), "credential"
				switch kind {
				case "value":
					value = make([]byte, secretstore.MaxValueBytes+extra)
				case "path":
					path = strings.Repeat("a", 1024+extra)
				case "token":
					writer.Token = func(context.Context) ([]byte, error) { return []byte(strings.Repeat("a", (64<<10)+extra)), nil }
				}
				err := writer.Put(t.Context(), "step-one", path, value)
				if (err == nil) != (extra == 0) {
					t.Fatal("writer bound", err)
				}
				if extra != 0 && len(backend.Calls) != 0 {
					t.Fatal("oversized writer input reached store")
				}
			})
		}
	}
}

func TestFetchValueAndPathCountBounds(t *testing.T) {
	for _, kind := range []string{"value", "paths"} {
		for _, extra := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/%d", kind, extra), func(t *testing.T) {
				fetchBoundCase(t, kind, extra)
			})
		}
	}
}

func fetchBoundCase(t *testing.T, kind string, extra int) {
	t.Helper()
	writer, backend := boundedWriter(t)
	count, size := 1, 5
	if kind == "paths" {
		count = 32 + extra
	} else {
		size = secretstore.MaxValueBytes + extra
	}
	backend.Resources = map[string]map[string]any{}
	paths := make([]string, count)
	for i := range paths {
		paths[i] = fmt.Sprintf("secret-%d", i)
		backend.Resources["/oberth/steps/step-one/"+paths[i]] = map[string]any{"value": base64.StdEncoding.EncodeToString(make([]byte, size))}
	}
	claim := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"system:serviceaccount:oberth-runs:step-one"}`))
	values, err := writer.Client.Fetch(t.Context(), config(), []byte("header."+claim+".signature"), paths)
	if (err == nil) != (extra == 0) {
		t.Fatal("fetch bound", err)
	}
	if extra == 0 && len(values) != count {
		t.Fatal("accepted set incomplete")
	}
	if extra != 0 && values != nil {
		t.Fatal("refusal published partial secrets")
	}
	if kind == "paths" && extra != 0 && len(backend.Calls) != 0 {
		t.Fatal("excess paths reached store")
	}
}
