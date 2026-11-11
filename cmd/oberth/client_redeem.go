package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/invite"
)

func readPrivateInvite(path string) (invite.File, error) {
	var result invite.File
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // G304: explicitly selected local private handoff file; no symlinks.
	if err != nil {
		return result, errors.New("cannot open the private invite file")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return result, errors.New("invite file must be a regular owner-only file (0600)")
	}
	body, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
	if err != nil || len(body) > 16<<10 {
		return result, errors.New("cannot read the private invite file (maximum 16 KiB)")
	}
	defer clear(body)
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
		return result, errors.New("invalid invite file")
	}
	if invite.ValidateIdentity(result.Identity) != nil || invite.ValidateURL(result.ServerURL) != nil || invite.ValidatePin(result.CAPin) != nil {
		return result, errors.New("invalid invite binding")
	}
	if _, err := invite.SecretDigest(result.Secret); err != nil {
		return result, errors.New("invalid invite file")
	}
	return result, nil
}
func validateInviteCertificate(raw string, csr *x509.CertificateRequest, identity string) error {
	block, rest := pem.Decode([]byte(raw))
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("invalid enrollment certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return errors.New("invalid enrollment certificate")
	}
	actual, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		return err
	}
	wanted, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return err
	}
	now := time.Now()
	if !bytes.Equal(actual, wanted) || leaf.Subject.CommonName != identity || leaf.IsCA || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		return errors.New("enrollment certificate does not match the local CSR or client identity")
	}
	return nil
}
func runClientRedeem(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("client redeem", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	filePath := flags.String("invite-file", "", "private 0600 invitation file; its contents never enter argv")
	csrPath := flags.String("csr", "", "local PEM CSR file (private key stays local)")
	sshPath := flags.String("ssh-public-key", "", "local SSH public key file")
	certPath := flags.String("certificate-out", "", "new certificate file; also writes .ca.crt and .url public connection files")
	if err := parseCommandFlags(flags, arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			printFlagUsage(flags)
			return nil
		}
		return usageError(err)
	}
	if flags.NArg() != 0 || *filePath == "" || *csrPath == "" || *sshPath == "" || *certPath == "" {
		return fmt.Errorf("%w: client redeem --invite-file <path> --csr <path> --ssh-public-key <path> --certificate-out <new-file>", errUsage)
	}
	invitation, err := readPrivateInvite(*filePath)
	if err != nil {
		return err
	}
	csrRaw, err := readBoundedFile(*csrPath, 64<<10)
	if err != nil {
		return errors.New("cannot read local CSR")
	}
	csr, err := clientcert.ParseCSR(csrRaw)
	if err != nil {
		return err
	}
	sshKey, err := readBoundedFile(*sshPath, 16<<10)
	if err != nil {
		return errors.New("cannot read local SSH public key")
	}
	if _, err := invite.SSHFingerprint(sshKey); err != nil {
		return err
	}
	// Reserve every destination before consuming the invite. Only these newly
	// created public files are removed if the network operation is refused.
	absolute, err := filepath.Abs(*certPath)
	if err != nil {
		return err
	}
	paths := []string{absolute, absolute + ".ca.crt", absolute + ".url"}
	files := make([]*os.File, 0, len(paths))
	consumed := false
	defer func() {
		for i, file := range files {
			_ = file.Close()
			if !consumed {
				_ = os.Remove(paths[i])
			}
		}
	}()
	for _, path := range paths {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // G304: explicit new public output files, reserved before redemption.
		if err != nil {
			return errors.New("certificate output files must be new; nothing was redeemed")
		}
		files = append(files, file)
	}
	issued, err := invite.Redeem(ctx, invitation, string(csrRaw), string(sshKey))
	if err != nil {
		return err
	}
	consumed = true
	if err := validateInviteCertificate(issued.Certificate, csr, invitation.Identity); err != nil {
		return err
	}
	values := []string{issued.Certificate, issued.ServerCA, invitation.ServerURL + "\n"}
	for i, value := range values {
		if _, err = io.WriteString(files[i], value); err != nil {
			return errors.New("invite redeemed but client file save failed; keep the local key and ask the admin to reissue its certificate")
		}
		if err = files[i].Sync(); err != nil {
			return errors.New("invite redeemed but client file sync failed; ask the admin to reissue its certificate")
		}
	}
	_, err = fmt.Fprintf(output, "Invite redeemed for %s. Client certificate: %s. No bearer token was issued.\n", invitation.Identity, absolute)
	return err
}
