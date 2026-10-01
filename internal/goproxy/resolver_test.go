package goproxy

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type requestJournal struct {
	mu    sync.Mutex
	paths []string
}

func (j *requestJournal) record(path string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.paths = append(j.paths, path)
}

func (j *requestJournal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.paths...)
}

type resolverFixture struct {
	mirror, public, direct *requestJournal
	proxy, guard, ca       string
}

// The child gets an allowlisted environment and cold per-command caches. All
// resolver and forward-proxy endpoints are loopback TLS servers. The last one
// rejects every CONNECT; it can observe direct attempts but cannot forward any
// traffic. No actual public service or private repository is contacted.
func newResolverFixture(t *testing.T, mirrorHandler http.Handler, publicStatus int) resolverFixture {
	t.Helper()
	f := resolverFixture{mirror: &requestJournal{}, public: &requestJournal{}, direct: &requestJournal{}}
	ca, _, certPEM, keyPEM := selfSignedCA(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(handler http.Handler) string {
		s := httptest.NewUnstartedServer(handler)
		s.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
		s.StartTLS()
		t.Cleanup(s.Close)
		return s.URL
	}
	mirrorURL := serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mirror.record(r.URL.Path)
		mirrorHandler.ServeHTTP(w, r)
	}))
	publicURL := serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.public.record(r.URL.Path)
		if publicStatus != http.StatusOK {
			http.Error(w, "fixture public failure", publicStatus)
			return
		}
		servePublicFixture(t, w, r)
	}))
	f.guard = serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.direct.record(r.Method + " " + r.Host)
		http.Error(w, "external traffic forbidden by fixture", http.StatusForbidden)
	}))
	f.proxy = mirrorURL + "," + publicURL + "|direct"
	f.ca = filepath.Join(t.TempDir(), "public-ca.pem")
	if err := os.WriteFile(f.ca, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func servePublicFixture(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	const mod = "public.example.invalid/issue764-fixture"
	const gomod = "module " + mod + "\n\ngo 1.22\n"
	if !strings.HasPrefix(r.URL.Path, "/"+mod+"/") {
		http.NotFound(w, r)
		return
	}
	switch strings.TrimPrefix(r.URL.Path, "/"+mod+"/") {
	case "@v/list":
		_, _ = fmt.Fprintln(w, "v1.0.0")
	case "@latest", "@v/v1.0.0.info":
		_, _ = fmt.Fprintln(w, `{"Version":"v1.0.0","Time":"2026-09-29T12:00:00Z"}`)
	case "@v/v1.0.0.mod":
		_, _ = fmt.Fprint(w, gomod)
	case "@v/v1.0.0.zip":
		var b bytes.Buffer
		z := zip.NewWriter(&b)
		for _, file := range []struct{ name, content string }{{"go.mod", gomod}, {"fixture.go", "package fixture\n"}} {
			dst, err := z.Create(mod + "@v1.0.0/" + file.name)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := fmt.Fprint(dst, file.content); err != nil {
				t.Error(err)
				return
			}
		}
		if err := z.Close(); err != nil {
			t.Error(err)
			return
		}
		_, _ = w.Write(b.Bytes())
	default:
		http.NotFound(w, r)
	}
}

func (f resolverFixture) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture.invalid/root\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, goBinary, args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + filepath.Dir(goBinary),
		"GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local", "GO111MODULE=on",
		"GOPATH=" + filepath.Join(dir, "gopath"), "GOMODCACHE=" + filepath.Join(dir, "gomod"),
		"GOCACHE=" + filepath.Join(dir, "gocache"), "GOMAXPROCS=2", "GOFLAGS=-p=2 -modcacherw",
		"GOPROXY=" + f.proxy, "GONOPROXY=none", "GONOSUMDB=go.example.test",
		// This ambient setting must not turn private resolution into direct.
		"GOPRIVATE=go.example.test", "GOSUMDB=off", "GOVCS=*:off", "GOTELEMETRY=off",
		"HTTPS_PROXY=" + f.guard, "HTTP_PROXY=" + f.guard, "NO_PROXY=127.0.0.1",
		"SSL_CERT_FILE=" + f.ca, "SSL_CERT_DIR=" + dir,
	}
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("Go command exceeded fixture deadline: %s", output)
	}
	return string(output), err
}

func TestGoResolverPrivateRouting(t *testing.T) {
	const private = "go.example.test/issue764-fixture"
	for _, tt := range []struct {
		name, fail string
		args       []string
		wantPath   string
		ok         bool
	}{
		{"download", "", []string{"mod", "download", "-json", private + "@v1.0.0"}, "/@v/v1.0.0.zip", true},
		{"latest", "", []string{"list", "-m", "-json", private + "@latest"}, "/@v/list", true},
		{"list", "", []string{"list", "-m", "-versions", private}, "/@v/list", true},
		{"missing module latest", "", []string{"list", "-m", "go.example.test/unknown-fixture@latest"}, "/@v/list", false},
		{"missing module version", "", []string{"mod", "download", "go.example.test/unknown-fixture@v1.0.0"}, "/@v/v1.0.0.info", false},
		{"missing module list", "", []string{"list", "-m", "-versions", "go.example.test/unknown-fixture"}, "/@v/list", false},
		{"nested", "", []string{"list", "-m", private + "/nested@latest"}, "/@v/list", false},
		{"root", "", []string{"list", "-m", "go.example.test@latest"}, "/@v/list", false},
		{"missing version", "", []string{"mod", "download", private + "@v1.0.1"}, "/@v/v1.0.1.info", false},
		{"branch unsupported", "", []string{"list", "-m", private + "@main"}, "/@v/main.info", false},
		{"escaped case", "", []string{"list", "-m", "go.example.test/UnknownFixture@latest"}, "/!unknown!fixture/@v/list", false},
		{"list backend", "list", []string{"list", "-m", "-versions", private}, "/@v/list", false},
		{"info backend", "info", []string{"mod", "download", private + "@v1.0.0"}, "/@v/v1.0.0.info", false},
		{"mod backend", "mod", []string{"mod", "download", private + "@v1.0.0"}, "/@v/v1.0.0.mod", false},
		{"zip backend", "zip", []string{"mod", "download", private + "@v1.0.0"}, "/@v/v1.0.0.zip", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newResolverFixture(t, routingFixture(tt.fail), http.StatusNotFound)
			output, err := f.run(t, tt.args...)
			if (err == nil) != tt.ok {
				t.Fatalf("Go error %v; output %s", err, output)
			}
			if !strings.Contains(strings.Join(f.mirror.all(), "\n"), tt.wantPath) {
				t.Fatalf("Go did not exercise %q: mirror %v; output %s", tt.wantPath, f.mirror.all(), output)
			}
			if len(f.public.all()) != 0 || len(f.direct.all()) != 0 {
				t.Fatalf("private coordinates escaped: public %v direct %v", f.public.all(), f.direct.all())
			}
		})
	}
}

func TestGoResolverLatestEndpoint(t *testing.T) {
	// With no tags, Go invokes @latest after list. Its denial must remain
	// terminal as well; an empty list is not permission to try public lookup.
	h := NewHandler(&mockCache{}, "go.example.test", map[string]string{"issue764-fixture": "fixture"})
	f := newResolverFixture(t, h, http.StatusNotFound)
	output, err := f.run(t, "list", "-m", "go.example.test/issue764-fixture@latest")
	if err == nil || !strings.Contains(strings.Join(f.mirror.all(), "\n"), "/@latest") {
		t.Fatalf("latest missing: %v mirror %v output %s", err, f.mirror.all(), output)
	}
	if len(f.public.all()) != 0 || len(f.direct.all()) != 0 {
		t.Fatalf("private latest escaped: %v %v", f.public.all(), f.direct.all())
	}
}

func TestGoResolverPublicFallback(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusGone, http.StatusBadGateway} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newResolverFixture(t, routingFixture(""), status)
			output, err := f.run(t, "mod", "download", "-json", "public.example.invalid/issue764-fixture@v1.0.0")
			if len(f.mirror.all()) == 0 || len(f.public.all()) == 0 {
				t.Fatalf("fallback not exercised: %s", output)
			}
			if status == http.StatusOK {
				if err != nil || len(f.direct.all()) != 0 || !strings.Contains(output, `"Sum": "h1:`) {
					t.Fatalf("public download: %v %s direct %v", err, output, f.direct.all())
				}
			} else if err == nil || len(f.direct.all()) == 0 {
				t.Fatalf("public error did not attempt guarded direct fallback: %v %s", err, output)
			}
		})
	}
}

func TestGoResolverOriginal404Control(t *testing.T) {
	// Original routing error: a private miss returned 404. This deliberate
	// control proves the same actual Go command reaches both fake fallbacks.
	f := newResolverFixture(t, http.HandlerFunc(http.NotFound), http.StatusNotFound)
	_, err := f.run(t, "mod", "download", "go.example.test/unknown-fixture@v1.0.0")
	if err == nil || len(f.public.all()) == 0 || len(f.direct.all()) == 0 {
		t.Fatalf("original 404 control did not escape: error %v public %v direct %v", err, f.public.all(), f.direct.all())
	}
}

func TestGoResolverUntrustedMirrorStops(t *testing.T) {
	f := newResolverFixture(t, routingFixture(""), http.StatusNotFound)
	otherCA, _, _, _ := selfSignedCA(t)
	if err := os.WriteFile(f.ca, otherCA, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := f.run(t, "mod", "download", "go.example.test/issue764-fixture@v1.0.0")
	if err == nil || !strings.Contains(output, "certificate signed by unknown authority") {
		t.Fatalf("untrusted mirror was not refused: %v %s", err, output)
	}
	if len(f.public.all()) != 0 || len(f.direct.all()) != 0 {
		t.Fatalf("TLS failure escaped: public %v direct %v", f.public.all(), f.direct.all())
	}
}
