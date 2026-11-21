package contracttest

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"golang.org/x/crypto/ssh"
)

type ScannerFixture struct {
	Port                            app.HostScanner
	Address, Algorithm, Fingerprint string
}

func HostScanner(t *testing.T, setup func(*testing.T) ScannerFixture) {
	t.Helper()
	t.Run("host_scanner", func(t *testing.T) {
		f := setup(t)
		key, err := f.Port.Scan(context.Background(), f.Address, f.Algorithm)
		if err != nil || ssh.FingerprintSHA256(key) != f.Fingerprint {
			t.Fatal("wrong offered key", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err = f.Port.Scan(ctx, f.Address, f.Algorithm); err == nil {
			t.Fatal("scanner ignored cancellation")
		}
	})
}
