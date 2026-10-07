package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/secretstore"
)

const identityRoot = "/run/oberth-identities"

func identityStoreEnabled() bool { return os.Getenv("OBERTH_IDENTITY_STORE") == "openbao" }

type baoIdentityStore struct {
	client    *secretstore.Client
	namespace string
}

func newBaoIdentityStore(namespace string) (*baoIdentityStore, error) {
	var ca []byte
	if path := os.Getenv("VAULT_CACERT"); path != "" {
		var err error
		ca, err = readBoundedFile(path, 4<<20)
		if err != nil {
			return nil, fmt.Errorf("read identity store trust anchor: %w", err)
		}
	}
	client, err := secretstore.New(secretstore.Config{
		Address: os.Getenv("VAULT_ADDR"), Role: os.Getenv("OBERTH_VAULT_ROLE"), CACertPEM: ca,
		AuthMountPath:     os.Getenv("OBERTH_VAULT_AUTH_MOUNT"),
		AllowInsecureHTTP: os.Getenv("OBERTH_VAULT_INSECURE_DEV_HTTP") == "true",
	})
	if err != nil {
		return nil, err
	}
	return &baoIdentityStore{client: client, namespace: namespace}, nil
}

func (s *baoIdentityStore) Load(ctx context.Context, name string) (map[string][]byte, error) {
	data, _, err := s.client.ReadIdentity(ctx, s.namespace, name)
	return data, err
}

// Save preserves every existing field. Replacing an identity needs a separate,
// explicit rotation flow; concurrent writes are refused through KV CAS.
func (s *baoIdentityStore) Save(ctx context.Context, name string, values map[string][]byte) error {
	data, version, err := s.client.ReadIdentity(ctx, s.namespace, name)
	if err != nil {
		return err
	}
	if data == nil {
		data = map[string][]byte{}
	}
	defer func() {
		for _, v := range data {
			clear(v)
		}
	}()
	for key, value := range values {
		if old := data[key]; len(old) > 0 && !bytes.Equal(old, value) && key != "known_hosts" {
			return errors.New("identity already exists; refusing replacement")
		}
		data[key] = bytes.Clone(value)
	}
	if err = s.client.WriteIdentity(ctx, s.namespace, name, version, data); err != nil {
		return err
	}
	return materializeIdentity(name, data)
}

func materializeIdentity(name string, data map[string][]byte) error {
	// The chart owns this tmpfs; pin and validate it before touching secrets.
	root, err := prepareSecretExecRoot(identityRoot)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	dir := map[string]string{"server": "server", "oberth-upstream-key": "upstream", "oberth-known-hosts": "hosts"}[name]
	if dir == "" {
		return errors.New("unknown runtime identity bundle")
	}
	target := filepath.Join(identityRoot, dir)
	if info, err := os.Lstat(target); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("identity directory is not a real directory")
	}
	if err := os.MkdirAll(target, 0700); err != nil {
		return err
	}
	for key, value := range data {
		if key == "" || key == "." || key == ".." || filepath.Base(key) != key || strings.ContainsAny(key, "\\\x00") {
			return errors.New("invalid identity filename")
		}
		file, err := os.CreateTemp(target, ".identity-")
		if err != nil {
			return err
		}
		temp := file.Name()
		_, err = file.Write(value)
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(temp, filepath.Join(target, key))
		}
		if err != nil {
			_ = os.Remove(temp)
			return err
		}
	}
	return nil
}

func bootstrapRuntimeIdentities(ctx context.Context, options *serveOptions) error {
	root, err := prepareSecretExecRoot(identityRoot)
	if err != nil {
		return err
	}
	if err := root.Close(); err != nil {
		return err
	}
	s, err := newBaoIdentityStore(options.namespace)
	if err != nil {
		return err
	}
	data, err := s.Load(ctx, "server")
	if err != nil {
		return err
	}
	if data == nil {
		data, err = generateRuntimeIdentity(options.namespace, strings.Split(os.Getenv("OBERTH_TLS_NAMES"), ","))
		if err != nil {
			return err
		}
		if err = s.Save(ctx, "server", data); err != nil {
			return err
		}
	}
	defer func() {
		for _, v := range data {
			clear(v)
		}
	}()
	if _, err := tls.X509KeyPair(data["tls.crt"], data["tls.key"]); err != nil {
		return errors.New("stored server TLS identity is invalid")
	}
	if _, err := ssh.ParsePrivateKey(data["ssh_host_key"]); err != nil {
		return errors.New("stored server SSH identity is invalid")
	}
	if err = materializeIdentity("server", data); err != nil {
		return err
	}
	for _, name := range []string{"oberth-upstream-key", "oberth-known-hosts"} {
		values, err := s.Load(ctx, name)
		if err != nil {
			return err
		}
		if values != nil {
			err = materializeIdentity(name, values)
			for _, v := range values {
				clear(v)
			}
			if err != nil {
				return err
			}
		}
	}
	options.tlsCert = identityRoot + "/server/tls.crt"
	options.tlsKey = identityRoot + "/server/tls.key"
	options.sshHostKey = identityRoot + "/server/ssh_host_key"
	options.upstreamKey = identityRoot + "/upstream/id_ed25519"
	options.knownHosts = identityRoot + "/hosts/known_hosts"
	if options.argoGoProxyCert != "" {
		options.argoGoProxyCert = options.tlsCert
		options.argoGoProxyKey = options.tlsKey
		options.argoGoProxyCA = options.tlsCert
	}
	return nil
}

func generateRuntimeIdentity(namespace string, extra []string) (map[string][]byte, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "oberth"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	names := []string{
		"oberth", "oberth." + namespace, "oberth." + namespace + ".svc", "oberth." + namespace + ".svc.cluster.local",
		"oberth-goproxy", "oberth-goproxy." + namespace, "oberth-goproxy." + namespace + ".svc", "oberth-goproxy." + namespace + ".svc.cluster.local",
	}
	for _, name := range append(names, extra...) {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if ip := net.ParseIP(name); ip != nil {
			cert.IPAddresses = append(cert.IPAddresses, ip)
		} else {
			cert.DNSNames = append(cert.DNSNames, name)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	defer clear(keyDER)
	_, host, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	defer clear(host)
	hostPEM, err := ssh.MarshalPrivateKey(host, "oberth host")
	if err != nil {
		return nil, err
	}
	return map[string][]byte{"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), "ssh_host_key": pem.EncodeToMemory(hostPEM)}, nil
}
