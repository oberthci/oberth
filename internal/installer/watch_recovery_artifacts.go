package installer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/mod/semver"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Same public trust pin as .oberth/pins/release-cosign.pub. No caller key.
const watchReleasePublicKey = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEORWOI3V47dAPvCBNec5aDeoGd0my
SxOZVlFj9e3OyEC2KD00sLeZoLKH4hewaY/+dmzYk1iuZXwNW56YmnNz2A==
-----END PUBLIC KEY-----
`

func boundedWatchFile(name string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return nil, errors.New("watch input requires an absolute clean path")
	}
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- explicit approved artifact/credential path; nofollow, regular-file bounds and stable descriptor/name identity checked below.
	if err != nil {
		return nil, errors.New("cannot open bounded watch input")
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > limit || before.Size() < 0 {
		return nil, errors.New("watch input size or type differs")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("watch input exceeds bound")
	}
	after, err := f.Stat()
	named, e := os.Lstat(name)
	if err != nil || e != nil || !os.SameFile(before, after) || !os.SameFile(after, named) || after.Size() != int64(len(raw)) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !named.Mode().IsRegular() {
		return nil, errors.New("watch input changed while reading")
	}
	return raw, nil
}
func watchFile(f watchPublicFile, limit int64) ([]byte, error) {
	if len(f.SHA256) != 64 || !isSHA256Digest("sha256:"+f.SHA256) {
		return nil, errors.New("invalid public artifact digest")
	}
	raw, err := boundedWatchFile(f.Path, limit)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != f.SHA256 {
		return nil, errors.New("public watch artifact digest differs")
	}
	return raw, nil
}
func verifyWatchBundle(payload, bundle []byte) error {
	block, rest := pem.Decode([]byte(watchReleasePublicKey))
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("invalid source release key")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return errors.New("invalid source release key")
	}
	pub, ok := key.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("unsupported source release key")
	}
	return verifyWatchBundleKey(payload, bundle, pub)
}
func verifyWatchBundleKey(payload, bundle []byte, key *ecdsa.PublicKey) error {
	var b struct {
		MediaType            string `json:"mediaType"`
		VerificationMaterial struct {
			PublicKey struct {
				Hint string `json:"hint"`
			} `json:"publicKey"`
		} `json:"verificationMaterial"`
		MessageSignature struct {
			MessageDigest struct {
				Algorithm string `json:"algorithm"`
				Digest    string `json:"digest"`
			} `json:"messageDigest"`
			Signature string `json:"signature"`
		} `json:"messageSignature"`
	}
	if len(bundle) > 32768 || strictWatchDecode(bundle, &b) != nil || b.MediaType != "application/vnd.dev.sigstore.bundle.v0.3+json" || b.MessageSignature.MessageDigest.Algorithm != "SHA2_256" {
		return errors.New("unsupported detached release signature bundle")
	}
	digest, err := base64.StdEncoding.Strict().DecodeString(b.MessageSignature.MessageDigest.Digest)
	signature, e := base64.StdEncoding.Strict().DecodeString(b.MessageSignature.Signature)
	sum := sha256.Sum256(payload)
	if err != nil || e != nil || !bytes.Equal(digest, sum[:]) || !ecdsa.VerifyASN1(key, sum[:], signature) {
		return errors.New("public release signature invalid")
	}
	return nil
}

func watchBinaryDigest(name string) (string, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- selected public executable; nofollow regular descriptor, bounded hash, matched to its approved digest by callers.
	if err != nil {
		return "", errors.New("cannot bind executable")
	}
	defer func() { _ = f.Close() }()
	return watchDescriptorDigest(f)
}

// /proc/self/exe intentionally follows the kernel's running-inode link. An
// atomic replacement of the installation pathname cannot select different bytes.
func watchRunningExecutableDigest() (string, error) {
	f, err := os.Open("/proc/self/exe")
	if err != nil {
		return "", errors.New("cannot bind running executable")
	}
	defer func() { _ = f.Close() }()
	return watchDescriptorDigest(f)
}

func watchDescriptorDigest(f *os.File) (string, error) {
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > 256<<20 {
		return "", errors.New("executable size or type differs")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (256<<20)+1))
	after, e := f.Stat()
	if err != nil || e != nil || n != before.Size() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", errors.New("executable changed while binding")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Freeze the reviewed signed archive and selected TLS identity for both preview
// and normal Helm. Neither caller chart overrides nor ambient kubeconfig remain.
func prepareWatchRecovery(ctx context.Context, cfg Config, deps Deps) (Config, Deps, func(), error) {
	p, err := readWatchRecoveryPlan(cfg)
	if err != nil {
		return cfg, deps, func() {}, err
	}
	if p == nil {
		return cfg, deps, func() {}, nil
	}
	if cfg.watchRecovery != nil {
		return cfg, deps, func() {}, errors.New("watch recovery target already prepared")
	}
	if runtime.GOOS != "linux" || cfg.ChartPath != "" || deps.RestConfig == nil || !strings.HasPrefix(deps.RestConfig.Host, "https://") || deps.RestConfig.Insecure || deps.RestConfig.ExecProvider != nil || deps.RestConfig.AuthProvider != nil {
		return cfg, deps, func() {}, errors.New("watch recovery requires fixed Linux HTTPS credentials")
	}
	if err = requireWatchRecoveryValues(cfg, *p); err != nil {
		return cfg, deps, func() {}, err
	}
	a := p.Artifacts
	record, err := watchFile(a.Record, 262144)
	if err != nil {
		return cfg, deps, func() {}, err
	}
	bundle, err := watchFile(a.RecordBundle, 32768)
	if err != nil || verifyWatchBundle(record, bundle) != nil {
		return cfg, deps, func() {}, errors.New("signed release record binding failed")
	}
	var release struct {
		SchemaVersion int               `json:"schemaVersion"`
		Component     string            `json:"component"`
		Version       string            `json:"version"`
		Artifacts     map[string]string `json:"artifacts"`
		Chart         struct {
			Digest  string `json:"digest"`
			Version string `json:"version"`
			GARRef  string `json:"garRef"`
		} `json:"chart"`
		Images struct {
			Server struct {
				Ref string `json:"ref"`
			} `json:"server"`
		} `json:"images"`
		Source struct {
			SHA        string `json:"sha"`
			Repository string `json:"repository"`
			Created    string `json:"created"`
		} `json:"source"`
	}
	if strictWatchDecode(record, &release) != nil || release.SchemaVersion != 1 || release.Component != "oberth" || canonicalChartVersion(release.Version) != canonicalChartVersion(p.ChartVersion) || semver.Compare(canonicalChartVersion(release.Version), "v0.16.25") <= 0 || release.Source.SHA != a.SourceSHA || release.Source.Repository != "github.com/oberthci/oberth" || release.Images.Server.Ref != a.ServerImage || release.Chart.Digest != "sha256:"+a.Chart.SHA256 || canonicalChartVersion(release.Chart.Version) != canonicalChartVersion(p.ChartVersion) {
		return cfg, deps, func() {}, errors.New("signed successor release scope differs")
	}
	binName := "oberth-" + runtime.GOOS + "-" + runtime.GOARCH
	if release.Artifacts[binName] != "sha256:"+a.InstallerSHA256 {
		return cfg, deps, func() {}, errors.New("signed installer digest differs")
	}
	sum, err := watchRunningExecutableDigest()
	if err != nil || sum != a.InstallerSHA256 {
		return cfg, deps, func() {}, errors.New("running installer is not signed successor")
	}
	checksums, err := watchFile(a.Checksums, 65536)
	if err != nil {
		return cfg, deps, func() {}, err
	}
	bundle, err = watchFile(a.ChecksumsBundle, 32768)
	if err != nil || verifyWatchBundle(checksums, bundle) != nil {
		return cfg, deps, func() {}, errors.New("signed checksums binding failed")
	}
	if err = validateWatchChecksums(checksums, binName, a.InstallerSHA256); err != nil {
		return cfg, deps, func() {}, err
	}
	chart, err := watchFile(a.Chart, 8<<20)
	if err != nil {
		return cfg, deps, func() {}, err
	}
	bundle, err = watchFile(a.ChartBundle, 32768)
	if err != nil || verifyWatchBundle(chart, bundle) != nil {
		return cfg, deps, func() {}, errors.New("signed chart binding failed")
	}
	chartFD, err := sealedWatchValues(chart)
	if err != nil {
		return cfg, deps, func() {}, err
	}
	closeFiles := []*os.File{chartFD}
	scratch := ""
	cleanup := func() {
		for _, f := range closeFiles {
			_ = f.Close()
		}
		if scratch != "" {
			_ = os.RemoveAll(scratch)
		}
	}
	frozenConfig, err := freezeWatchRecoveryConfig(deps.RestConfig)
	if err != nil {
		cleanup()
		return cfg, deps, func() {}, err
	}
	frozenClient, err := kubernetes.NewForConfig(frozenConfig)
	if err != nil {
		cleanup()
		return cfg, deps, func() {}, errors.New("cannot construct frozen recovery client")
	}
	deps.RestConfig, deps.KubeClient = frozenConfig, frozenClient
	frozenKube, err := watchRecoveryKubeconfig(frozenConfig)
	if err != nil {
		cleanup()
		return cfg, deps, func() {}, err
	}
	defer clear(frozenKube)
	kubeFD, err := sealedWatchValues(frozenKube)
	if err != nil {
		cleanup()
		return cfg, deps, func() {}, err
	}
	closeFiles = append(closeFiles, kubeFD)
	helmpath, err := exec.LookPath("helm")
	if err != nil {
		cleanup()
		return cfg, deps, func() {}, errors.New("helm unavailable")
	}
	helmpath, err = filepath.EvalSymlinks(helmpath)
	if err != nil {
		cleanup()
		return cfg, deps, func() {}, errors.New("helm path differs")
	}
	helm, err := boundedWatchFile(helmpath, 128<<20)
	if err != nil {
		cleanup()
		return cfg, deps, func() {}, err
	}
	h := sha256.Sum256(helm)
	if hex.EncodeToString(h[:]) != a.HelmSHA256 {
		cleanup()
		return cfg, deps, func() {}, errors.New("reviewed Helm digest differs")
	}
	scratch, err = os.MkdirTemp("/var/tmp", "oberth-watch-public-helm-")
	if err != nil {
		cleanup()
		return cfg, deps, func() {}, errors.New("cannot freeze public Helm")
	}
	binary := filepath.Join(scratch, "helm")
	if os.WriteFile(binary, helm, 0500) != nil { // #nosec G306 -- public hash-bound executable needs owner execute; enclosing fresh directory is 0700 and no group/other access is granted.
		cleanup()
		return cfg, deps, func() {}, errors.New("cannot freeze public Helm")
	}
	runner := func(ctx context.Context, args []string) ([]byte, error) {
		return runBoundedRecoveryHelm(ctx, binary, args)
	}
	version, err := runner(ctx, []string{"version", "--template", "{{.Version}}"})
	if err != nil || strings.TrimSpace(string(version)) != "v4.2.3" {
		cleanup()
		return cfg, deps, func() {}, errors.New("watch recovery requires exact Helm v4.2.3")
	}
	cfg.watchRecovery = p
	cfg.watchChart = fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), chartFD.Fd())
	cfg.ImageRef = a.ServerImage
	kubePath := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), kubeFD.Fd())
	deps.RunHelm = HelmWithKubeArgs(runner, []string{"--kubeconfig", kubePath, "--kube-context", "watch-recovery"})
	return cfg, deps, cleanup, nil
}

func validateWatchChecksums(raw []byte, name, digest string) error {
	seen := map[string]bool{}
	found := false
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 || !isSHA256Digest("sha256:"+fields[0]) || strings.ContainsAny(fields[1], "/\\") || seen[fields[1]] {
			return errors.New("invalid signed artifact checksums")
		}
		seen[fields[1]] = true
		if fields[1] == name {
			found = fields[0] == digest
		}
	}
	if !found {
		return errors.New("signed checksums omit installer")
	}
	return nil
}

func freezeWatchRecoveryConfig(c *rest.Config) (*rest.Config, error) {
	if c == nil || c.Insecure || c.Impersonate.UserName != "" || c.Impersonate.UID != "" || len(c.Impersonate.Groups) != 0 || len(c.Impersonate.Extra) != 0 || c.Transport != nil || c.WrapTransport != nil || c.Proxy != nil || c.Dial != nil || c.AuthProvider != nil || c.AuthConfigPersister != nil || c.ExecProvider != nil || c.BearerTokenFile != "" || c.Username != "" || c.Password != "" || c.APIPath != "" || c.NegotiatedSerializer != nil || len(c.NextProtos) != 0 {
		return nil, errors.New("watch recovery refuses impersonation and custom or dynamic authority")
	}
	u, err := url.Parse(c.Host)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("watch recovery requires a literal HTTPS endpoint")
	}
	read := func(raw []byte, path string) ([]byte, error) {
		if len(raw) != 0 {
			if len(raw) > 1<<20 {
				return nil, errors.New("watch credential exceeds bound")
			}
			return append([]byte(nil), raw...), nil
		}
		if path != "" {
			return boundedWatchFile(path, 1<<20)
		}
		return nil, nil
	}
	ca, err := read(c.CAData, c.CAFile)
	if err != nil {
		return nil, err
	}
	cert, err := read(c.CertData, c.CertFile)
	if err != nil {
		return nil, err
	}
	key, err := read(c.KeyData, c.KeyFile)
	if err != nil {
		return nil, err
	}
	if !watchPublicCA(map[string]string{"ca.crt": string(ca)}) || len(c.BearerToken) > 1<<20 || (len(cert) == 0) != (len(key) == 0) || len(cert) == 0 && c.BearerToken == "" || len(cert) != 0 && c.BearerToken != "" {
		clear(key)
		return nil, errors.New("watch recovery requires bounded fixed trust and one literal credential")
	}
	if len(cert) != 0 {
		if _, err = tls.X509KeyPair(cert, key); err != nil {
			clear(key)
			return nil, errors.New("watch recovery literal client credential is invalid")
		}
	}
	requestTimeout := c.Timeout
	if requestTimeout <= 0 || requestTimeout > 15*time.Second {
		requestTimeout = 15 * time.Second
	}
	return &rest.Config{Host: c.Host, BearerToken: c.BearerToken, TLSClientConfig: rest.TLSClientConfig{CAData: ca, CertData: cert, KeyData: key, ServerName: c.ServerName}, Timeout: requestTimeout, QPS: c.QPS, Burst: c.Burst, UserAgent: c.UserAgent, DisableCompression: c.DisableCompression,
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }}, nil
}

func watchRecoveryKubeconfig(c *rest.Config) ([]byte, error) {
	if c == nil || c.CAFile != "" || c.CertFile != "" || c.KeyFile != "" {
		return nil, errors.New("watch recovery client is not frozen")
	}
	config := clientcmdapi.Config{APIVersion: "v1", Kind: "Config", CurrentContext: "watch-recovery", Clusters: map[string]*clientcmdapi.Cluster{"watch-recovery": {Server: c.Host, CertificateAuthorityData: c.CAData, TLSServerName: c.ServerName}}, AuthInfos: map[string]*clientcmdapi.AuthInfo{"watch-recovery": {ClientCertificateData: c.CertData, ClientKeyData: c.KeyData, Token: c.BearerToken}}, Contexts: map[string]*clientcmdapi.Context{"watch-recovery": {Cluster: "watch-recovery", AuthInfo: "watch-recovery"}}}
	return clientcmd.Write(config)
}
func runBoundedRecoveryHelm(ctx context.Context, binary string, args []string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdout := &watchPreviewBuffer{limit: 8 << 20, cancel: cancel}
	stderr := &watchPreviewBuffer{limit: 64 << 10, cancel: cancel}
	defer func() { clear(stderr.Bytes()) }()
	cmd := exec.CommandContext(ctx, binary, args...) // #nosec G204 -- exact hash-bound public binary and installer-built argv; no shell.
	// Explicit flags and the frozen kubeconfig must not be overridden by
	// ambient Helm/Kubernetes tokens, impersonation, proxy, driver or plugins.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "GOMAXPROCS=2", "HELM_NO_PLUGINS=1", "HELM_DRIVER=secret"}
	for _, name := range []string{"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		if value := os.Getenv(name); filepath.IsAbs(value) && filepath.Clean(value) == value {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		clear(stdout.Bytes())
		return nil, errors.New("reviewed Helm operation failed or exceeded bound")
	}
	return stdout.Bytes(), nil
}
