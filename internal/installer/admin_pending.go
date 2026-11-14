package installer

import (
	"bytes"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/client"
)

type pendingInstallerEnrollment struct {
	Target installerCredentialTarget `json:"target"`
	Key    []byte                    `json:"key"`
	CSR    []byte                    `json:"csr"`
}

func installerEnrollmentRoot(deps Deps) (string, error) {
	root, err := deps.clientConfigRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	return root, client.TrustClientFiles(root)
}

func reusePendingInstallerEnrollment(cfg *Config, deps Deps) error {
	root, err := installerEnrollmentRoot(deps)
	if err != nil {
		return err
	}
	name := pendingInstallerName(*cfg, deps)
	path := filepath.Join(root, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
		return errors.New("unsafe pending installer enrollment")
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	body, err := directory.ReadFile(name)
	if err != nil {
		return err
	}
	defer clear(body)
	var pending pendingInstallerEnrollment
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&pending) != nil {
		return errors.New("invalid pending installer enrollment")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		clear(pending.Key)
		return errors.New("trailing pending installer enrollment data")
	}
	if pending.Target != credentialTarget(*cfg, deps) {
		clear(pending.Key)
		return errors.New("pending installer enrollment belongs to another Kubernetes target")
	}
	if err := validateInstallerEnrollment(pending.Key, pending.CSR); err != nil {
		clear(pending.Key)
		return err
	}
	if cfg.enrollment != nil {
		clear(cfg.enrollment.key)
	}
	if cfg.enrollment == nil {
		cfg.enrollment = &installerEnrollment{}
	}
	*cfg.enrollment = installerEnrollment{key: pending.Key, csr: pending.CSR}
	return nil
}

func validateInstallerEnrollment(key, csr []byte) error {
	keyBlock, _ := pem.Decode(key)
	csrBlock, _ := pem.Decode(csr)
	if keyBlock == nil || csrBlock == nil {
		return errors.New("invalid pending installer key or CSR")
	}
	private, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return errors.New("invalid pending installer key")
	}
	signer, ok := private.(crypto.Signer)
	if !ok {
		return errors.New("invalid pending installer signing key")
	}
	request, err := x509.ParseCertificateRequest(csrBlock.Bytes)
	if err != nil || request.CheckSignature() != nil {
		return errors.New("invalid pending installer CSR")
	}
	public, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil || !bytes.Equal(public, request.RawSubjectPublicKeyInfo) {
		return errors.New("pending installer key does not match CSR")
	}
	return nil
}

func persistPendingInstallerEnrollment(cfg *Config, deps Deps) error {
	if err := reusePendingInstallerEnrollment(cfg, deps); err != nil {
		return err
	}
	if cfg.enrollment == nil || validateInstallerEnrollment(cfg.enrollment.key, cfg.enrollment.csr) != nil {
		return errors.New("installer enrollment key is missing or invalid")
	}
	root, err := installerEnrollmentRoot(deps)
	if err != nil {
		return err
	}
	body, err := json.Marshal(pendingInstallerEnrollment{Target: credentialTarget(*cfg, deps), Key: cfg.enrollment.key, CSR: cfg.enrollment.csr})
	if err != nil {
		return err
	}
	defer clear(body)
	err = publishPendingInstallerEnrollment(root, pendingInstallerName(*cfg, deps), body)
	if errors.Is(err, os.ErrExist) {
		return reusePendingInstallerEnrollment(cfg, deps)
	}
	return err
}

func syncConfigDirectory(path string) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// Publish only once: simultaneous installers must converge on the same key.
func publishPendingInstallerEnrollment(path, name string, body []byte) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	file, err := os.CreateTemp(path, ".oberth-pending-*")
	if err != nil {
		return err
	}
	tmp := filepath.Base(file.Name())
	defer func() { _ = root.Remove(tmp) }()
	_, err = file.Write(body)
	if err == nil {
		err = file.Sync()
	}
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	if err := root.Link(tmp, name); err != nil {
		return err
	}
	return syncConfigDirectory(path)
}

func pendingInstallerName(cfg Config, deps Deps) string {
	target, _ := json.Marshal(credentialTarget(cfg, deps))
	sum := sha256.Sum256(target)
	return fmt.Sprintf("installer-pending-%x.json", sum)
}
