package verifytest

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/releaseverify"
)

// WithKey builds only the consumer test binary with a fixture embed overlay.
// The canonical file and the product verifier have no runtime key override.
// Rebuilds require direct go test so caller build overlays remain recoverable.
func WithKey(m *testing.M) int {
	if !testing.Testing() {
		return 1
	}
	key, err := fixturePublicKey()
	if err != nil {
		return fixtureFailure(err)
	}
	current, _ := releaseverify.PublicKey()
	fixture := filepath.Base(os.Args[0]) == "oberth-fixture.test"
	if fixture && bytes.Equal(current, key) {
		return m.Run()
	}
	if fixture {
		return fixtureFailure(errors.New("test binary did not embed its fixture pin"))
	}
	flag.Parse()
	limit, err := time.ParseDuration(flag.Lookup("test.timeout").Value.String())
	if err != nil || limit <= 0 {
		return fixtureFailure(errors.New("fixture tests require a positive test timeout"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	err = runFixture(ctx, key)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err != nil {
		return fixtureFailure(err)
	}
	return 0
}

func fixturePublicKey() ([]byte, error) {
	key, err := fixtureKey()
	if err != nil {
		return nil, err
	}
	raw, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: raw}), nil
}
func fixtureFailure(err error) int {
	_, _ = fmt.Fprintln(os.Stderr, "release test fixture:", err)
	return 1
}
