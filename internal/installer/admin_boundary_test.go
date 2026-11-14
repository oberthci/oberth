package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/httpstream"
	streamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/testtimeout"
)

func TestAdminHostBoundary(t *testing.T) {
	t.Run("EveryOperationUsesHTTP", testAdminHostEveryOperationUsesHTTP)
	t.Run("AuthenticatedKubernetesUpgrade", testAdminHostAuthenticatedKubernetesUpgrade)
	t.Run("PortForwardCancellationBoundsHandshake", testAdminHostPortForwardCancellationBoundsHandshake)
	t.Run("FailedDownloadRemovesPartial", testAdminHostFailedDownloadRemovesPartial)
	t.Run("OneFirstAdminExecWithPublicInput", testAdminHostOneFirstAdminExecWithPublicInput)
	t.Run("CachedEnrollmentStaysWithTarget", testAdminHostCachedEnrollmentStaysWithTarget)
	t.Run("RecordedAndExplicitContext", testAdminHostRecordedAndExplicitContext)
	t.Run("ServerExecInventory", testAdminHostServerExecInventory)
}
func testAdminHostEveryOperationUsesHTTP(t *testing.T) {
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("missing admin credential")
		}
		action := strings.TrimPrefix(r.URL.Path, "/api/admin/")
		calls[action]++
		if action == "database-backup" {
			_, _ = io.WriteString(w, "verified archive")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		value := `{}`
		switch action {
		case "upstream-list":
			value = `{"upstreams":[]}`
		case "access-list":
			value = `{"grants":[]}`
		case "public-identity":
			value = `{"content":"public"}`
		case "version":
			value = `{"version":"v1","schema":1}`
		}
		_, _ = io.WriteString(w, value)
	}))
	defer server.Close()
	remote, err := client.New(t.Context(), client.Config{BaseURL: server.URL, Token: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientset()
	kube.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "exec" {
			t.Error("server exec attempted")
		}
		return true, nil, errors.New("unexpected Kubernetes action")
	})
	deps := Deps{Admin: remote, KubeClient: kube, RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Error("host process attempted")
		return nil, errors.New("process refused")
	}}
	for _, action := range []string{"upstream-list", "upstream-add", "upstream-provide-key", "repo-add", "repo-verify", "uplink-add", "public-identity", "readiness", "idle", "audit-record", "access-list", "version"} {
		var result any
		if err := CallAdmin(t.Context(), Config{}, deps, action, map[string]any{}, &result); err != nil {
			t.Fatal(action, err)
		}
	}
	path := filepath.Join(t.TempDir(), "database.tar")
	if _, err := snapshotServerDatabase(t.Context(), backupDeps{Deps: deps}, "oberth", path); err != nil {
		t.Fatal(err)
	}
	if calls["database-backup"] != 1 || len(kube.Actions()) != 0 {
		t.Fatal(calls, kube.Actions())
	}
}

func testAdminHostAuthenticatedKubernetesUpgrade(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-kube-token" {
			t.Error("Kubernetes credential missing")
		}
		if _, err := httpstream.Handshake(r, w, []string{"portforward.k8s.io"}); err != nil {
			t.Error(err)
			return
		}
		connection := streamspdy.NewResponseUpgrader().UpgradeResponse(w, r, httpstream.NoOpNewStreamHandler)
		if connection == nil {
			return
		}
		defer func() { _ = connection.Close() }()
		select {
		case <-connection.CloseChan():
		case <-stop:
		}
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL + "/portforward")
	if err != nil {
		t.Fatal(err)
	}
	config := &rest.Config{Host: server.URL, BearerToken: "fixture-kube-token", TLSClientConfig: rest.TLSClientConfig{CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}}
	dialer, err := newAdminPortForwardDialer(t.Context(), config, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	connection, protocol, err := dialer.Dial("portforward.k8s.io")
	if err != nil {
		t.Fatal(err)
	}
	if protocol != "portforward.k8s.io" {
		t.Error(protocol)
	}
	_ = connection.Close()
}

func testAdminHostPortForwardCancellationBoundsHandshake(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"server"},"status":{"phase":"Running"}}]}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/portforward") {
			t.Error("unexpected Kubernetes operation", r.URL.Path)
		}
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	defer server.CloseClientConnections()
	config := &rest.Config{Host: server.URL}
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, cleanup, err := adminAPI(ctx, Config{enrollment: &installerEnrollment{result: api.FirstAdminResult{InstallerCertificate: "public fixture"}}}, Deps{KubeClient: kube, RestConfig: config})
		cleanup()
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(testtimeout.Start(5 * time.Second)):
		cancel()
		server.CloseClientConnections()
		t.Fatal("port-forward handshake not started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation lost", err)
		}
	case <-time.After(testtimeout.Start(5 * time.Second)):
		server.CloseClientConnections()
		t.Fatal("port-forward handshake ignored caller cancellation")
	}
}

func testAdminHostFailedDownloadRemovesPartial(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "partial.tar")
	deps := backupDeps{Deps: Deps{Admin: streamingFixtureAdmin{download: func(_ context.Context, _ int64, output io.Writer) error {
		_, _ = io.WriteString(output, "partial")
		return io.ErrUnexpectedEOF
	}}}}
	if _, err := snapshotServerDatabase(t.Context(), deps, "oberth", destination); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("download failure lost", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial archive retained", err)
	}
}

func testAdminHostOneFirstAdminExecWithPublicInput(t *testing.T) {
	enrollment, err := newInstallerEnrollment()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(enrollment.key)
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	deps := Deps{ContextName: "recorded", ClientConfigRoot: func() (string, error) { return root, nil }, RunCommand: func(_ context.Context, body []byte, name string, args ...string) ([]byte, error) {
		calls++
		joined := strings.Join(args, " ")
		if name != "kubectl" || !strings.Contains(joined, "--context recorded") || !strings.HasSuffix(joined, "admin create-first-admin") {
			t.Fatal(name, args)
		}
		if bytes.Contains(body, []byte("PRIVATE KEY")) {
			t.Fatal("private key crossed exec boundary")
		}
		var request api.FirstAdminRequest
		if json.Unmarshal(body, &request) != nil || request.InstallerCSR == "" || request.Identity != "admin@host" {
			t.Fatal("invalid public enrollment")
		}
		return json.Marshal(api.FirstAdminResult{InstallerCertificate: "public cert", ServerCA: "public CA"})
	}}
	_, err = createFirstAdmin(t.Context(), Config{enrollment: enrollment}, deps, []byte("public SSH key"), "admin@host")
	if err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	for _, name := range []string{"installer-client.key", "installer-client.crt", "installer-ca.crt"} {
		st, err := os.Stat(filepath.Join(root, name))
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatal(name, err)
		}
	}
}

func testAdminHostCachedEnrollmentStaysWithTarget(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	enrollment, err := newInstallerEnrollment()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(enrollment.key)
	files, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(files["ca.key"])
	authority, err := clientcert.Load(files["ca.crt"], files["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	certificate, _, err := authority.Issue("admin@host", enrollment.csr, 0)
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{ContextName: "selected", ClientConfigRoot: func() (string, error) { return root, nil }, RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
		return json.Marshal(api.FirstAdminResult{InstallerCertificate: string(certificate), ServerCA: string(files["ca.crt"])})
	}}
	if _, err := createFirstAdmin(t.Context(), Config{enrollment: enrollment}, deps, []byte("public"), "admin@host"); err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := reuseInstallerEnrollment(&cfg, deps); err != nil || cfg.enrollment.result.InstallerCertificate == "" {
		t.Fatal("matching credential not reused", err)
	}
	defer clear(cfg.enrollment.key)
	deps.ContextName = "other"
	cfg = Config{}
	if err := reuseInstallerEnrollment(&cfg, deps); err != nil || cfg.enrollment.result.InstallerCertificate != "" {
		t.Fatal("cached credential crossed targets", err)
	}
}

func testAdminHostRecordedAndExplicitContext(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	cfg := clientcmdapi.NewConfig()
	cfg.CurrentContext = "ambient"
	for _, name := range []string{"ambient", "recorded"} {
		cfg.Clusters[name] = &clientcmdapi.Cluster{Server: "https://" + name + ".invalid"}
		cfg.Contexts[name] = &clientcmdapi.Context{Cluster: name}
	}
	path := filepath.Join(root, "kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	before, _ := os.ReadFile(path)
	dir, err := client.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := recordKubernetesTarget(Config{Namespace: "chosen", KubeconfigPath: path}, Deps{ContextName: "recorded", ClientConfigRoot: func() (string, error) { return dir, nil }}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args []string
		want string
	}{{[]string{"get", "pods"}, "recorded"}, {[]string{"--context", "ambient", "get", "pods"}, "ambient"}} {
		args, _, _, cleanup, err := pinnedKubernetesCommand("kubectl", test.args, os.Environ())
		if err != nil {
			t.Fatal(err)
		}
		private, err := clientcmd.LoadFromFile(args[1])
		if err != nil {
			t.Fatal(err)
		}
		if private.CurrentContext != test.want {
			t.Fatal(private.CurrentContext, test.want)
		}

		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
	}
	_, rest, selected, err := loadKubeConfigForContext("", "", io.Discard)
	if err != nil || selected != "recorded" || rest.Host != "https://recorded.invalid" {
		t.Fatal(selected, err)
	}
	for _, args := range [][]string{{"--context=", "get", "pods"}, {"--context", "", "get", "pods"}} {
		if _, _, _, cleanup, err := pinnedKubernetesCommand("kubectl", args, os.Environ()); err == nil {
			_ = cleanup()
			t.Fatal("empty explicit context accepted")
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("source kubeconfig changed")
	}
}

func testAdminHostServerExecInventory(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		if path != "admin_api.go" && (strings.Contains(s, `"/usr/local/bin/oberth"`) && strings.Contains(s, `"exec"`)) {
			t.Errorf("server exec outside first-admin transport: %s", path)
		}
		if strings.Contains(s, `serverContainerName, false`) || strings.Contains(s, `serverContainerName, true`) {
			t.Errorf("server container exec: %s", path)
		}
	}
}
