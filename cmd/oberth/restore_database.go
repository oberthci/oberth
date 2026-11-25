package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"path/filepath"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

// The cluster restore Pod receives the authenticated canonical database on stdin.
func (a application) restoreDatabase(ctx context.Context, args []string) int {
	f := flag.NewFlagSet("restore-database", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	destination := f.String("destination", "", "Absent database path.")
	options := stateport.Options{Schema: store.Schema}
	f.StringVar(&options.Protocol.SchemaIdentity, "schema-identity", "", "Schema identity.")
	f.StringVar(&options.Protocol.AuditDomain, "audit-domain", "", "Audit domain.")
	f.Int64Var(&options.MaxBytes, "max-database-bytes", 0, "Logical database byte limit.")
	if err := f.Parse(args); err != nil || f.NArg() != 0 || !filepath.IsAbs(*destination) {
		return a.failed(installationProblem("Database restore input is invalid.", "Use the installation restore command with a verified backup."))
	}
	backup, err := store.NewBackup(*destination)
	if err != nil {
		return a.failed(err)
	}
	report, err := backup.Restore(ctx, stateport.RestoreRequest{Options: options, Destination: *destination}, a.in)
	if err != nil {
		return a.failed(err)
	}
	if err = json.NewEncoder(a.out).Encode(report); err != nil {
		return a.failed(err)
	}
	return 0
}
