package main

import (
	"context"
	"encoding/json"
	"errors"
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

func runUplinkCert(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	flags, connection, parse := adminCLIFlags("uplink cert", args)
	path := flags.String("csr", "", "public PEM CSR file, or - for stdin")
	fingerprint := flags.String("fingerprint", "", "SSH fingerprint to disambiguate")
	ttl := flags.Duration("ttl", 0, "certificate lifetime; zero uses the server default")
	positional, help, err := parse()
	if err != nil {
		return err
	}
	if help {
		return commandFlagHelp(flags, "Usage: oberth uplink cert <identity> --csr <file|->")
	}
	if len(positional) != 1 || *path == "" {
		return fmt.Errorf("%w: uplink cert <identity> --csr <file|->", errUsage)
	}
	csr, err := readClientCSR(*path, input)
	if err != nil {
		return err
	}
	arguments := map[string]any{"identity": positional[0], "fingerprint": *fingerprint, "csr": string(csr)}
	if *ttl != 0 {
		arguments["ttl"] = ttl.String()
	}
	raw, err := existingAdminTool(ctx, connection, "uplink_cert", arguments)
	if err != nil {
		return err
	}
	var result struct {
		Certificate string `json:"certificate"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Certificate == "" {
		return errors.New("invalid public certificate response")
	}
	_, err = io.WriteString(output, result.Certificate)
	return err
}
