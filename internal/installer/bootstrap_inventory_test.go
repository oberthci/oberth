package installer

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Only the bootstrap contract's private go test -outputdir consumes these
// records. Ordinary fixture runs need no inventory destination.
var bootstrapLeakControl = flag.Bool("bootstrap-credential-leak-control", false, "adverse fixture control; private bootstrap captures only")

func bootstrapCredentialInventory(t *testing.T, values ...[]byte) {
	t.Helper()
	output := flag.Lookup("test.outputdir")
	if output == nil || output.Value.String() == "" {
		return
	}
	writeBootstrapInventory(t, output.Value.String(), values...)
}

func writeBootstrapInventory(t *testing.T, directory string, values ...[]byte) {
	t.Helper()
	file, err := os.CreateTemp(directory, "installer-credential-*.json")
	if err != nil {
		t.Fatal("cannot create private fixture inventory")
	}
	record := struct {
		Test   string   `json:"test"`
		Values [][]byte `json:"values"`
	}{strings.Split(t.Name(), "/")[0], values}
	err = json.NewEncoder(file).Encode(record)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal("cannot write private fixture inventory")
	}
}

func bootstrapPrivateKey(t *testing.T, key any) {
	t.Helper()
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal("cannot inventory private fixture key")
	}
	bootstrapCredentialInventory(t, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}))
	if ec, ok := key.(*ecdsa.PrivateKey); ok {
		raw, err = x509.MarshalECPrivateKey(ec)
		if err != nil {
			t.Fatal("cannot inventory EC fixture key")
		}
		bootstrapCredentialInventory(t, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: raw}))
	}
}

func bootstrapPrivateFiles(t *testing.T, directory string) {
	t.Helper()
	output := flag.Lookup("test.outputdir")
	if output == nil || output.Value.String() == "" {
		return
	}
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > 1<<20 {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if block, _ := pem.Decode(raw); block != nil && strings.Contains(block.Type, "PRIVATE KEY") {
			bootstrapCredentialInventory(t, raw)
		}
		return nil
	})
	if err != nil {
		t.Fatal("cannot inventory fixture files")
	}
}

func bootstrapInstallerEnrollments(t *testing.T, directory string) {
	t.Helper()
	output := flag.Lookup("test.outputdir")
	if output == nil || output.Value.String() == "" {
		return
	}
	paths, err := filepath.Glob(filepath.Join(directory, "installer-pending-*.json"))
	if err != nil {
		t.Fatal("cannot inventory pending enrollment")
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("cannot read private enrollment fixture")
		}
		var pending pendingInstallerEnrollment
		if json.Unmarshal(raw, &pending) != nil || len(pending.Key) == 0 {
			t.Fatal("cannot decode private enrollment fixture")
		}
		bootstrapCredentialInventory(t, pending.Key)
		clear(pending.Key)
		clear(raw)
	}
}
