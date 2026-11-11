package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/client"
)

func runClientRenew(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("client renew", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	connectionFlags := registerTransportFlags(flags)
	ttl := flags.String("ttl", "", "new certificate lifetime (default 2160h; 1h–8760h)")
	outputDir := flags.String("output-dir", "", "new identity directory for unmanaged client files; default: atomically rotate the shared current symlink")
	if err := parseCommandFlags(flags, arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			printFlagUsage(flags)
			return nil
		}
		return usageError(err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: client renew takes no arguments", errUsage)
	}
	connection, err := client.ResolveConnection(*connectionFlags)
	if err != nil {
		return err
	}
	if connection.ClientCert == "" || connection.ClientKey == "" {
		return errors.New("renewal requires a current client certificate and key")
	}
	certPath, keyPath := connection.ClientCert, connection.ClientKey
	parent := filepath.Dir(certPath)
	managed := false
	if filepath.Dir(keyPath) == parent && filepath.Base(parent) == "current" {
		if info, err := os.Lstat(parent); err == nil && info.Mode()&os.ModeSymlink != 0 {
			marker, readErr := os.ReadFile(filepath.Join(parent, ".oberth-client-identity")) //nolint:gosec // G304: operator-selected client directory; only the fixed public marker is read.
			managed = readErr == nil && string(marker) == "oberth-client-identity-v1\n"
		}
	}
	if *outputDir == "" && !managed {
		return errors.New("unmanaged client files: pass --output-dir for the new pair, or use the plugin's shared current identity symlink")
	}
	directory := *outputDir
	if directory != "" {
		if err := os.Mkdir(directory, 0700); err != nil {
			return err
		}
	} else {
		directory, err = os.MkdirTemp(filepath.Dir(parent), "identity-")
		if err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(directory, ".oberth-client-identity"), []byte("oberth-client-identity-v1\n"), 0600); err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(directory)
		}
	}()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	defer clear(raw)
	keyFile := filepath.Join(directory, filepath.Base(keyPath))
	certFile := filepath.Join(directory, filepath.Base(certPath))
	if keyFile == certFile {
		return errors.New("client certificate and key must have distinct filenames")
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}), 0600); err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "client.csr"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), 0600); err != nil {
		return err
	}
	response, err := mcpRequest(ctx, mcpFlagResult{mcpConfig: mcpConfig{URL: connection.MCPURL, CAFile: connection.CACert, ClientCert: certPath, ClientKey: keyPath}}, "tools/call", map[string]any{"name": "client_renew", "arguments": map[string]string{"csr": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})), "ttl": *ttl}})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return errors.New("client certificate renewal was refused by the server")
	}
	var result struct {
		IsError    bool            `json:"isError"`
		Structured json.RawMessage `json:"structuredContent"`
		Content    []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return err
	}
	if result.IsError {
		return errors.New("client certificate renewal was refused by the server")
	}
	var issued struct {
		Certificate string `json:"certificate"`
	}
	if len(result.Structured) > 0 {
		err = json.Unmarshal(result.Structured, &issued)
	} else if len(result.Content) > 0 {
		err = json.Unmarshal([]byte(result.Content[0].Text), &issued)
	}
	if err != nil || issued.Certificate == "" {
		return errors.New("renewal returned no certificate")
	}
	if err := os.WriteFile(certFile, []byte(issued.Certificate), 0600); err != nil {
		return err
	}
	if _, err := client.LoadClientCertificate(certFile, keyFile); err != nil {
		return err
	}
	// Publish only the complete validated pair. Existing connections and old
	// pins remain valid while the next handshake loads this pair.
	if *outputDir == "" {
		link := directory + ".link"
		target, err := filepath.Abs(directory)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, link); err != nil {
			return err
		}
		defer func() { _ = os.Remove(link) }()
		if err := os.Rename(link, parent); err != nil {
			return err
		}
	}
	keep = true
	_, err = fmt.Fprintf(output, "Client certificate renewed; new identity saved in %s. The previous certificate remains valid until its expiry.\n", directory)
	return err
}
