package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestEveryStoreSetupDetectsPendingRootAuthorization(t *testing.T) {
	for _, mode := range []string{"ordinary", "delegated-backup", "quorum-backup"} {
		t.Run(mode, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(config+".backup-setup.json", []byte("public-operation-receipt"), 0600); err != nil {
				t.Fatal(err)
			}
			flags := flag.NewFlagSet("setup", flag.ContinueOnError)
			flags.String("config", config, "")
			flags.Bool("backup-only", mode != "ordinary", "")
			share := ""
			if mode == "quorum-backup" {
				share = "unread-share"
			}
			flags.String("unseal-file", share, "")
			err := runStoreOperator(t.Context(), installationRequest{name: "secretstore setup", flags: flags}, io.Discard)
			if err == nil || publicerr.Classify(err).Reason() != "Store root authorization needs cleanup." {
				t.Fatal("setup changed authority without checking the pending receipt", err)
			}
		})
	}
}
