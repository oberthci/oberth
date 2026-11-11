package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

func readClientCSR(path string, input io.Reader) ([]byte, error) {
	if path == "-" {
		raw, err := io.ReadAll(io.LimitReader(input, (64<<10)+1))
		if err != nil || len(raw) > 64<<10 {
			return nil, errors.New("cannot read CSR (maximum 64 KiB)")
		}
		return raw, nil
	}
	return readBoundedFile(path, 64<<10)
}

func issueUplinkCertificate(ctx context.Context, database *store.Store, identity, fingerprint string, csr []byte, ttl time.Duration) (string, error) {
	authority, err := loadClientAuthority()
	if err != nil {
		return "", err
	}
	return service.IssueClientCert(ctx, &service.ClientCertIssuer{Authority: authority, Store: database}, administrativeActor, identity, fingerprint, csr, ttl)
}

func runUplinkCert(ctx context.Context, arguments []string, input io.Reader, output io.Writer, deps uplinkDependencies) error {
	flags := flag.NewFlagSet("uplink cert", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("csr", "", "PEM CSR file or - for stdin (private key remains local)")
	fingerprint := flags.String("fingerprint", "", "SSH fingerprint to disambiguate the uplink")
	ttl := flags.Duration("ttl", 0, "certificate lifetime (default 2160h; 1h–8760h)")
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod)")
	if err := parseCommandFlags(flags, arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			printFlagUsage(flags)
			return nil
		}
		return usageError(err)
	}
	if flags.NArg() != 1 || *path == "" {
		return fmt.Errorf("%w: uplink cert <identity> --csr <file|->", errUsage)
	}
	csr, err := readClientCSR(*path, input)
	if err != nil {
		return err
	}
	if deps.mutationGate == nil {
		return errors.New("admin audit mutation gate unavailable")
	}
	if err := deps.mutationGate(ctx, "uplink.cert.issue", *databasePath); err != nil {
		return err
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, configuredStoreOptions(ctx))
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	cert, err := issueUplinkCertificate(ctx, database, flags.Arg(0), *fingerprint, csr, *ttl)
	if err != nil {
		return err
	}
	_, err = io.WriteString(output, cert)
	return err
}
