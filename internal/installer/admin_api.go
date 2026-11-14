package installer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/portforward"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/kubeconfig"
)

// AdminAPI is the one administrative transport used by host-side callers.
type AdminAPI interface {
	Admin(context.Context, string, any, any) error
	DownloadAdminBackup(context.Context, int64, io.Writer) error
}

type installerCredentialTarget struct {
	Context   string `json:"context"`
	Namespace string `json:"namespace"`
	Server    string `json:"server"`
	CASHA256  string `json:"ca_sha256"`
}

func credentialTarget(cfg Config, deps Deps) installerCredentialTarget {
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	server := ""
	if deps.RestConfig != nil {
		server = deps.RestConfig.Host
	}
	hash, _ := clusterCAHash(deps.RestConfig)
	return installerCredentialTarget{Context: deps.ContextName, Namespace: ns, Server: server, CASHA256: hash}
}

type installerEnrollment struct {
	key, csr []byte
	result   api.FirstAdminResult
}

func newInstallerEnrollment() (*installerEnrollment, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	defer clear(private)
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, err
	}
	defer clear(der)
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, private)
	if err != nil {
		return nil, err
	}
	return &installerEnrollment{key: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), csr: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})}, nil
}

// resolveAdminClientConfig loads credentials without contacting Kubernetes or the server.
func resolveAdminClientConfig(cfg Config, deps Deps) (client.Config, error) {
	if cfg.enrollment == nil || cfg.enrollment.result.InstallerCertificate == "" {
		if err := reuseInstallerEnrollment(&cfg, deps); err != nil {
			return client.Config{}, err
		}
	}
	if cfg.enrollment != nil && cfg.enrollment.result.InstallerCertificate != "" {
		ns := cfg.Namespace
		if ns == "" {
			ns = DefaultNamespace
		}
		return client.Config{BaseURL: "https://oberth." + ns + ".svc:8443", ClientCertificatePEM: []byte(cfg.enrollment.result.InstallerCertificate), ClientKeyPEM: cfg.enrollment.key, CACert: cfg.enrollment.result.ServerCA}, nil
	}
	connection, err := client.ResolveConnection(client.ConnectionFlags{})
	if err != nil {
		return client.Config{}, err
	}
	return connection.Config(), nil
}

func adminAPI(ctx context.Context, cfg Config, deps Deps) (AdminAPI, func(), error) {
	if deps.Admin != nil {
		return deps.Admin, func() {}, nil
	}
	configured, err := resolveAdminClientConfig(cfg, deps)
	if err != nil {
		return nil, func() {}, err
	}
	closeConnection := func() {}
	if deps.KubeClient != nil && deps.RestConfig != nil {
		if err := verifyRecordedCluster(deps.ContextName, deps.RestConfig); err != nil {
			return nil, closeConnection, err
		}
		ns := cfg.Namespace
		if ns == "" {
			ns = DefaultNamespace
		}
		pods, err := deps.KubeClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=oberth"})
		if err != nil {
			return nil, closeConnection, err
		}
		var pod *corev1.Pod
		for i := range pods.Items {
			candidate := &pods.Items[i]
			if candidate.DeletionTimestamp == nil && candidate.Status.Phase == corev1.PodRunning {
				if pod != nil {
					return nil, closeConnection, errors.New("admin API requires one running server Pod")
				}
				pod = candidate
			}
		}
		if pod == nil {
			return nil, closeConnection, errors.New("no running server Pod for admin API")
		}
		endpoint := deps.KubeClient.CoreV1().RESTClient().Post().Namespace(ns).Resource("pods").Name(pod.Name).SubResource("portforward").URL()
		dialer, err := newAdminPortForwardDialer(ctx, deps.RestConfig, endpoint)
		if err != nil {
			return nil, closeConnection, err
		}

		stop, ready, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		forwarder, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{"0:8443"}, stop, ready, io.Discard, io.Discard)
		if err != nil {
			return nil, closeConnection, err
		}
		go func() { done <- forwarder.ForwardPorts() }()
		select {
		case <-ready:
		case err := <-done:
			close(stop)
			return nil, closeConnection, fmt.Errorf("admin API connection: %w", err)
		case <-ctx.Done():
			close(stop)
			<-done
			return nil, closeConnection, ctx.Err()
		}
		ports, err := forwarder.GetPorts()
		if err != nil {
			close(stop)
			<-done
			return nil, closeConnection, err
		}
		closeConnection = func() { close(stop); <-done }
		configured.BaseURL = fmt.Sprintf("https://127.0.0.1:%d", ports[0].Local)
		configured.ServerName = "oberth." + ns + ".svc"
	}
	remote, err := client.New(ctx, configured)
	if err != nil {
		closeConnection()
		return nil, func() {}, err
	}
	return remote, closeConnection, nil
}

func CallAdmin(ctx context.Context, cfg Config, deps Deps, action string, args, into any) error {
	remote, closeConnection, err := adminAPI(ctx, cfg, deps)
	if err != nil {
		return err
	}
	defer closeConnection()
	return remote.Admin(ctx, action, args, into)
}

func readPublicIdentity(ctx context.Context, cfg Config, deps Deps, name string) ([]byte, error) {
	var result struct {
		Content string `json:"content"`
	}
	if err := CallAdmin(ctx, cfg, deps, "public-identity", map[string]string{"name": name}, &result); err != nil {
		return nil, err
	}
	return []byte(result.Content), nil
}

func createFirstAdmin(ctx context.Context, cfg Config, deps Deps, publicKey []byte, identity string) (api.FirstAdminResult, error) {
	if cfg.enrollment == nil {
		return api.FirstAdminResult{}, errors.New("installer enrollment key is missing")
	}
	if deps.ContextName == "" {
		return api.FirstAdminResult{}, errors.New("first admin requires the selected Kubernetes context")
	}
	if err := persistPendingInstallerEnrollment(&cfg, deps); err != nil {
		return api.FirstAdminResult{}, err
	}
	request := api.FirstAdminRequest{Identity: identity, SSHPublicKey: string(publicKey), CSR: string(cfg.adminCSR), InstallerCSR: string(cfg.enrollment.csr), Bearer: cfg.AdminBearer}
	body, err := json.Marshal(request)
	if err != nil {
		return api.FirstAdminResult{}, err
	}
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	args := []string{"--context", deps.ContextName, "-n", ns, "exec", "-i", "deploy/oberth", "-c", serverContainerName, "--", "/usr/local/bin/oberth", "admin", "create-first-admin"}
	raw, err := runFirstAdminExec(ctx, deps, body, args)
	defer clear(raw)
	if err != nil {
		return api.FirstAdminResult{}, err
	}
	var result api.FirstAdminResult
	if json.Unmarshal(raw, &result) != nil || result.InstallerCertificate == "" || result.ServerCA == "" {
		return result, errors.New("invalid first admin response")
	}
	cfg.enrollment.result = result
	// Recovery uses the same short-lived certificate. Nothing is placed in a Kubernetes Secret.
	root, err := deps.clientConfigRoot()
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return result, err
	}
	if err := client.TrustClientFiles(root); err != nil {
		return result, err
	}
	for name, data := range map[string][]byte{"installer-client.key": cfg.enrollment.key, "installer-client.crt": []byte(result.InstallerCertificate), "installer-ca.crt": []byte(result.ServerCA)} {
		if err := atomicWriteFile(filepath.Join(root, name), data, 0600); err != nil {
			return result, err
		}
	}
	target, err := json.Marshal(credentialTarget(cfg, deps))
	if err != nil {
		return result, err
	}
	if err := atomicWriteFile(filepath.Join(root, "installer-target.json"), target, 0600); err != nil {
		return result, err
	}
	return result, nil
}

func reuseInstallerEnrollment(cfg *Config, deps Deps) error {
	if cfg.enrollment == nil {
		cfg.enrollment = &installerEnrollment{}
	}
	root, err := deps.clientConfigRoot()
	if err != nil {
		return err
	}
	metadataPath := filepath.Join(root, "installer-target.json")
	info, err := os.Lstat(metadataPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
		return errors.New("unsafe installer target record")
	}
	if err := client.TrustClientFiles(root); err != nil {
		return err
	}
	metadata, err := os.ReadFile(metadataPath) //nolint:gosec // G304: fixed owner-only installer target record.
	if err != nil {
		return err
	}
	var recorded installerCredentialTarget
	if json.Unmarshal(metadata, &recorded) != nil {
		return errors.New("invalid installer target record")
	}
	if recorded != credentialTarget(*cfg, deps) {
		return nil
	}
	for _, name := range []string{"installer-client.key", "installer-client.crt", "installer-ca.crt"} {
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 128<<10 {
			return errors.New("installer credential must be an owner-only regular file")
		}
	}
	if err := client.TrustClientFiles(root); err != nil {
		return err
	}
	key, err := os.ReadFile(filepath.Join(root, "installer-client.key")) //nolint:gosec // G304: fixed credential name in validated owner-only configuration directory.
	if err != nil {
		return err
	}
	cert, err := os.ReadFile(filepath.Join(root, "installer-client.crt")) //nolint:gosec // G304: fixed credential name in validated owner-only configuration directory.
	if err != nil {
		clear(key)
		return err
	}
	ca, err := os.ReadFile(filepath.Join(root, "installer-ca.crt")) //nolint:gosec // G304: fixed credential name in validated owner-only configuration directory.
	if err != nil {
		clear(key)
		return err
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		clear(key)
		return errors.New("invalid cached installer credential")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		clear(key)
		return errors.New("invalid cached installer certificate")
	}
	if time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		clear(key)
		return nil
	}
	if _, err := client.LoadCAPool(string(ca)); err != nil {
		clear(key)
		return errors.New("invalid cached installer CA")
	}
	clear(cfg.enrollment.key)
	cfg.enrollment.key = key
	cfg.enrollment.result.InstallerCertificate = string(cert)
	cfg.enrollment.result.ServerCA = string(ca)
	return nil
}

func adminVersion(ctx context.Context, cfg Config, deps Deps) (string, int, error) {
	var result struct {
		Version string `json:"version"`
		Schema  int    `json:"schema"`
	}
	err := CallAdmin(ctx, cfg, deps, "version", nil, &result)
	return strings.TrimSpace(result.Version), result.Schema, err
}

type upstreamCheckResult struct {
	Upstreams []struct {
		Upstream struct {
			Name    string `json:"name"`
			BaseURL string `json:"base_url"`
		} `json:"upstream"`
		Auth string `json:"auth"`
	} `json:"upstreams"`
}

func upstreamCheckJSON(ctx context.Context, cfg Config, deps Deps) (upstreamCheckResult, error) {
	var result upstreamCheckResult
	err := CallAdmin(ctx, cfg, deps, "upstream-list", map[string]bool{"check": true}, &result)
	return result, err
}
func verifiedUpstreamAPI(result upstreamCheckResult, allowPending bool) bool {
	if len(result.Upstreams) == 0 {
		return false
	}
	for _, row := range result.Upstreams {
		if row.Auth != "ok" && !strings.HasPrefix(row.Auth, "ok (") && (!allowPending || !strings.HasPrefix(row.Auth, "pending:")) {
			return false
		}
	}
	return true
}

func AdminForTarget(ctx context.Context, cfg Config, action string, args, into any) error {
	contextName := cfg.ContextName
	path := cfg.KubeconfigPath
	if contextName == "" {
		target, err := RecordedKubernetesTarget()
		if err != nil {
			return err
		}
		contextName = target.Context
		if path == "" {
			path = target.Kubeconfig
		}
	}
	if contextName == "" {
		return errors.New("admin API cluster connection requires --context or the install record")
	}
	kube, restConfig, selected, err := loadKubeConfigWithRules(kubeconfig.Rules(path), contextName, "", io.Discard)
	if err != nil {
		return err
	}
	return CallAdmin(ctx, cfg, Deps{KubeClient: kube, RestConfig: restConfig, ContextName: selected}, action, args, into)
}
