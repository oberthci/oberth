//go:build ignore

// Publicly reviewed fixture helper. Private TLS material is created only in a
// distinct reserved process credential's verified tmpfs home by the admitted
// custody launcher, never by builds or ordinary tests.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	if run() != nil {
		fmt.Fprintln(os.Stderr, "synthetic watch fixture crypto refused")
		os.Exit(1)
	}
}
func run() error {
	uid, gid := os.Getuid(), os.Getgid()
	groups, e := os.Getgroups()
	cwd, err := os.Getwd()
	var fs unix.Statfs_t
	if uid < 1000000000 || uid >= 2000000000 || gid != uid || e != nil || len(groups) != 0 || err != nil || unix.Statfs(cwd, &fs) != nil || fs.Type != unix.TMPFS_MAGIC {
		return fmt.Errorf("custody differs")
	}
	info, err := os.Lstat(cwd)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("home differs")
	}
	var st unix.Stat_t
	if unix.Lstat(cwd, &st) != nil || int(st.Uid) != uid || int(st.Gid) != gid {
		return fmt.Errorf("home owner differs")
	}
	if unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) != nil {
		return fmt.Errorf("dumpability differs")
	}
	if unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}) != nil {
		return fmt.Errorf("core differs")
	}
	write := func(name string, raw []byte, mode os.FileMode) error {
		defer clear(raw)
		f, err := os.OpenFile(filepath.Join(cwd, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, mode)
		if err != nil {
			return err
		}
		_, err = f.Write(raw)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	type authority struct {
		cert *x509.Certificate
		key  *rsa.PrivateKey
	}
	serial := func() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)) }
	newCA := func(name string) (authority, error) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return authority{}, err
		}
		n, err := serial()
		if err != nil {
			return authority{}, err
		}
		c := &x509.Certificate{SerialNumber: n, Subject: pkix.Name{CommonName: "synthetic-watch-" + name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
		der, err := x509.CreateCertificate(rand.Reader, c, c, &key.PublicKey, key)
		if err != nil {
			return authority{}, err
		}
		c, err = x509.ParseCertificate(der)
		if err != nil {
			return authority{}, err
		}
		if err = write(name+"-ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
			return authority{}, err
		}
		return authority{c, key}, nil
	}
	leaf := func(ca authority, name string, server bool, client bool, admin bool) error {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return err
		}
		n, err := serial()
		if err != nil {
			return err
		}
		c := &x509.Certificate{SerialNumber: n, Subject: pkix.Name{CommonName: "synthetic-watch-" + name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment}
		if server {
			c.ExtKeyUsage = append(c.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
			c.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
			c.DNSNames = []string{"localhost"}
		}
		if client {
			c.ExtKeyUsage = append(c.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
		}
		if admin {
			c.Subject.Organization = []string{"system:masters"}
		}
		der, err := x509.CreateCertificate(rand.Reader, c, ca.cert, &key.PublicKey, ca.key)
		if err != nil {
			return err
		}
		if err = write(name+".crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
			return err
		}
		return write(name+".key", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600)
	}
	apiServer, err := newCA("api-serving")
	if err != nil {
		return err
	}
	apiClient, err := newCA("api-client")
	if err != nil {
		return err
	}
	etcdClient, err := newCA("etcd-client")
	if err != nil {
		return err
	}
	etcdPeer, err := newCA("etcd-peer")
	if err != nil {
		return err
	}
	for _, task := range []struct {
		ca                    authority
		name                  string
		server, client, admin bool
	}{{apiServer, "api-server", true, false, false}, {apiClient, "admin-client", false, true, true}, {etcdClient, "etcd-server", true, false, false}, {etcdClient, "api-etcd-client", false, true, false}, {etcdPeer, "etcd-peer", true, true, false}} {
		if err = leaf(task.ca, task.name, task.server, task.client, task.admin); err != nil {
			return err
		}
	}
	signer, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	if err = write("sa-signing.key", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(signer)}), 0600); err != nil {
		return err
	}
	public, err := x509.MarshalPKIXPublicKey(&signer.PublicKey)
	if err != nil {
		return err
	}
	return write("sa-signing.pub", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}), 0600)
}
