package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/term"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

const (
	// KindClusterName is the local cluster managed by oberth install on macOS.
	KindClusterName = "oberth"
	// KindContextName is the kubeconfig context created by kind for Oberth.
	KindContextName = "kind-" + KindClusterName

	kindCICachePath      = "/var/cache/oberth/ci"
	kindReleaseCachePath = "/var/cache/oberth/release"
)

// CommandRunner executes one host command. Input is connected to stdin when
// non-empty; commands are always invoked directly, never through a shell.
type CommandRunner func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error)

// InteractiveRunner connects one host command directly to the operator's
// terminal for pod-side interactive flows (kubectl exec -i). Commands are
// always invoked directly, never through a shell.
type InteractiveRunner func(ctx context.Context, name string, args ...string) error

// InstallDeps holds injectable host and cluster dependencies for the CLI-level
// install orchestration.
type InstallDeps struct {
	Output         io.Writer
	Input          io.Reader
	GOOS           string
	LookPath       func(string) (string, error)
	RunCommand     CommandRunner
	RunInteractive InteractiveRunner
	IsTerminal     func() bool
	UserCacheDir   func() (string, error)
	MkdirAll       func(string, fs.FileMode) error
	LoadKubeConfig func(contextName string) (kubernetes.Interface, *rest.Config, string, error)
	RunHelm        func(ctx context.Context, args []string) ([]byte, error)
}

func (deps InstallDeps) withDefaults() InstallDeps {
	if deps.Output == nil {
		deps.Output = io.Discard
	}
	if deps.Input == nil {
		deps.Input = os.Stdin
	}
	if deps.GOOS == "" {
		deps.GOOS = runtime.GOOS
	}
	if deps.LookPath == nil {
		deps.LookPath = exec.LookPath
	}
	if deps.RunCommand == nil {
		deps.RunCommand = DefaultRunCommand
	}
	if deps.RunInteractive == nil {
		deps.RunInteractive = DefaultRunInteractive
	}
	if deps.IsTerminal == nil {
		deps.IsTerminal = StdinIsTerminal
	}
	if deps.UserCacheDir == nil {
		deps.UserCacheDir = os.UserCacheDir
	}
	if deps.MkdirAll == nil {
		deps.MkdirAll = os.MkdirAll
	}
	if deps.LoadKubeConfig == nil {
		output := deps.Output
		deps.LoadKubeConfig = func(contextName string) (kubernetes.Interface, *rest.Config, string, error) {
			return LoadKubeConfigForContext(contextName, output)
		}
	}
	if deps.RunHelm == nil {
		deps.RunHelm = DefaultRunHelm
	}
	return deps
}

// Execute prepares the platform-selected Kubernetes cluster, loads its
// kubeconfig, and runs the cluster install flow.
func Execute(ctx context.Context, cfg Config, deps InstallDeps) error {
	deps = deps.withDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}

	contextName := ""
	kindClusterName := ""
	if deps.GOOS == "darwin" {
		kindClusterName = KindClusterName
		cfg.TLSExtraDNSNames, cfg.TLSExtraIPs = certificateNamesForKind(cfg.TLSExtraDNSNames, cfg.TLSExtraIPs)
		cacheRoot, err := deps.UserCacheDir()
		if err != nil {
			return fmt.Errorf("find macOS cache directory: %w", err)
		}
		config, _, err := kindClusterConfig(cacheRoot)
		if err != nil {
			return err
		}
		if cfg.DryRun {
			return printDryRunPlanWithKind(cfg, deps.Output, ClusterInfo{
				Context: KindContextName,
				Engine:  "kind",
				IsLocal: true,
			}, config)
		}

		var created bool
		contextName, created, err = prepareDarwinKind(ctx, cacheRoot, deps)
		if err != nil {
			return err
		}
		if created {
			_, _ = fmt.Fprintf(deps.Output, "Created kind cluster %q and selected context %q.\n", KindClusterName, contextName)
		} else {
			_, _ = fmt.Fprintf(deps.Output, "Using existing kind cluster %q with context %q.\n", KindClusterName, contextName)
		}
	}

	kubeClient, restConfig, selectedContext, err := deps.LoadKubeConfig(contextName)
	if err != nil {
		if contextName != "" {
			return fmt.Errorf("load kubeconfig context %q: %w", contextName, err)
		}
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	// Wire the production ReadPassword and MakeRaw defaults when the session
	// is interactive: x/term.ReadPassword and x/term.MakeRaw on the stdin fd.
	// Non-interactive sessions leave both nil (the callers check isInteractive
	// or fall back to text-based input).
	var readPw func() ([]byte, error)
	var makeRaw func() (func(), error)
	if deps.IsTerminal != nil && deps.IsTerminal() {
		readPw = func() ([]byte, error) {
			return term.ReadPassword(int(os.Stdin.Fd()))
		}
		makeRaw = func() (func(), error) {
			oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
			if err != nil {
				return nil, err
			}
			return func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }, nil
		}
	}

	// Top-level terminal restore safety net: if a panic or unhandled signal
	// occurs while the terminal is in raw mode, this deferred restore
	// ensures the terminal is returned to its previous state on any exit
	// path from Execute. The wrapper tracks the latest restore function
	// returned by MakeRaw so the defer always calls the right one.
	var terminalRestore func()
	if makeRaw != nil {
		originalMakeRaw := makeRaw
		makeRaw = func() (func(), error) {
			restore, err := originalMakeRaw()
			if err != nil {
				return restore, err
			}
			terminalRestore = restore
			return restore, nil
		}
	}
	defer func() {
		if terminalRestore != nil {
			terminalRestore()
		}
	}()

	return Run(ctx, cfg, Deps{
		Output:          deps.Output,
		Input:           deps.Input,
		RunHelm:         deps.RunHelm,
		RunCommand:      deps.RunCommand,
		RunInteractive:  deps.RunInteractive,
		IsTerminal:      deps.IsTerminal,
		KubeClient:      kubeClient,
		RestConfig:      restConfig,
		ContextName:     selectedContext,
		KindClusterName: kindClusterName,
		ReadPassword:    readPw,
		MakeRaw:         makeRaw,
	})
}

type kindConfig struct {
	Kind       string     `json:"kind"`
	APIVersion string     `json:"apiVersion"`
	Nodes      []kindNode `json:"nodes"`
}

type kindNode struct {
	Role              string            `json:"role"`
	ExtraPortMappings []kindPortMapping `json:"extraPortMappings"`
	ExtraMounts       []kindMount       `json:"extraMounts"`
}

type kindPortMapping struct {
	ContainerPort int    `json:"containerPort"`
	HostPort      int    `json:"hostPort"`
	ListenAddress string `json:"listenAddress"`
	Protocol      string `json:"protocol"`
}

type kindMount struct {
	HostPath      string `json:"hostPath"`
	ContainerPath string `json:"containerPath"`
}

// certificateNamesForKind adds the addresses a kind install is always reached
// on to whatever the operator named. The chart publishes fixed NodePorts bound
// to the loopback interface, so localhost and 127.0.0.1 are true of every kind
// install; naming them on the certificate is what lets the read-only CLI and an
// MCP client verify TLS without an /etc/hosts entry pointing a covered name at
// the loopback. Existing entries are preserved and never duplicated.
func certificateNamesForKind(dnsNames, ips []string) ([]string, []string) {
	return appendMissing(dnsNames, "localhost"), appendMissing(ips, "127.0.0.1")
}

func appendMissing(values []string, want string) []string {
	for _, value := range values {
		if value == want {
			return values
		}
	}
	return append(values, want)
}

func kindClusterConfig(userCacheDir string) ([]byte, []string, error) {
	if !filepath.IsAbs(userCacheDir) {
		return nil, nil, fmt.Errorf("macOS cache directory %q is not absolute", userCacheDir)
	}
	cleanCacheDir := filepath.Clean(userCacheDir)
	if cleanCacheDir == string(filepath.Separator) {
		return nil, nil, errors.New("refusing to use the filesystem root as the macOS cache directory")
	}
	cacheRoot := filepath.Join(cleanCacheDir, "oberth")
	cacheDirs := []string{
		filepath.Join(cacheRoot, "ci"),
		filepath.Join(cacheRoot, "release"),
	}
	config := kindConfig{
		Kind:       "Cluster",
		APIVersion: "kind.x-k8s.io/v1alpha4",
		Nodes: []kindNode{{
			Role: "control-plane",
			ExtraPortMappings: []kindPortMapping{
				{ContainerPort: 30022, HostPort: 30022, ListenAddress: "127.0.0.1", Protocol: "TCP"},
				{ContainerPort: 30443, HostPort: 30443, ListenAddress: "127.0.0.1", Protocol: "TCP"},
			},
			ExtraMounts: []kindMount{
				{HostPath: cacheDirs[0], ContainerPath: kindCICachePath},
				{HostPath: cacheDirs[1], ContainerPath: kindReleaseCachePath},
			},
		}},
	}
	encoded, err := yaml.Marshal(config)
	if err != nil {
		return nil, nil, fmt.Errorf("encode kind cluster config: %w", err)
	}
	return encoded, cacheDirs, nil
}

func prepareDarwinKind(ctx context.Context, cacheRoot string, deps InstallDeps) (string, bool, error) {
	deps = deps.withDefaults()
	if _, err := deps.LookPath("docker"); err != nil {
		return "", false, errors.New("docker CLI not found; install and start Docker Desktop (https://www.docker.com/products/docker-desktop/) or OrbStack (https://orbstack.dev/), then rerun oberth install")
	}
	if _, err := deps.LookPath("kind"); err != nil {
		return "", false, errors.New("kind not found; install it with `brew install kind`, then rerun oberth install")
	}
	if _, err := deps.LookPath("helm"); err != nil {
		return "", false, errors.New("helm not found; install it with `brew install helm`, then rerun oberth install")
	}
	if _, err := deps.LookPath("kubectl"); err != nil {
		return "", false, errors.New("kubectl not found; install it with `brew install kubernetes-cli` (or enable it in Docker Desktop), then rerun oberth install")
	}
	if output, err := deps.RunCommand(ctx, nil, "docker", "info"); err != nil {
		return "", false, fmt.Errorf("docker is not running; start Docker Desktop or OrbStack, wait until it is ready, then rerun oberth install: %w%s", err, commandOutputSuffix(output))
	}

	config, cacheDirs, err := kindClusterConfig(cacheRoot)
	if err != nil {
		return "", false, err
	}
	clusters, err := deps.RunCommand(ctx, nil, "kind", "get", "clusters")
	if err != nil {
		return "", false, fmt.Errorf("list kind clusters: %w%s", err, commandOutputSuffix(clusters))
	}
	for _, name := range strings.Fields(string(clusters)) {
		if name == KindClusterName {
			if err := validateExistingKindCluster(ctx, cacheDirs, deps); err != nil {
				return "", false, err
			}
			return KindContextName, false, nil
		}
	}

	for _, directory := range cacheDirs {
		if err := deps.MkdirAll(directory, 0o750); err != nil {
			return "", false, fmt.Errorf("create kind cache directory %s: %w", directory, err)
		}
	}
	_, _ = fmt.Fprintf(deps.Output, "Creating kind cluster %q with SSH/HTTPS port mappings and persistent cache mounts...\n", KindClusterName)
	output, err := deps.RunCommand(ctx, config, "kind", "create", "cluster", "--name", KindClusterName, "--config=-")
	if err != nil {
		return "", false, fmt.Errorf("create kind cluster %q: %w%s", KindClusterName, err, commandOutputSuffix(output))
	}
	return KindContextName, true, nil
}

type dockerContainerInspect struct {
	HostConfig struct {
		PortBindings map[string][]dockerPortBinding `json:"PortBindings"`
	} `json:"HostConfig"`
	Mounts []dockerMount `json:"Mounts"`
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type dockerMount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

func validateExistingKindCluster(ctx context.Context, cacheDirs []string, deps InstallDeps) error {
	containerName := KindClusterName + "-control-plane"
	output, err := deps.RunCommand(ctx, nil, "docker", "inspect", containerName)
	if err != nil {
		return fmt.Errorf("inspect existing kind control-plane container %q: %w%s", containerName, err, commandOutputSuffix(output))
	}

	var inspections []dockerContainerInspect
	if err := json.Unmarshal(output, &inspections); err != nil {
		return fmt.Errorf("decode docker inspection for existing kind cluster %q: %w", KindClusterName, err)
	}
	if len(inspections) != 1 {
		return fmt.Errorf("inspect existing kind cluster %q: expected one control-plane container, got %d", KindClusterName, len(inspections))
	}

	inspection := inspections[0]
	var incompatibilities []string
	for _, port := range []string{"30022", "30443"} {
		bindings := inspection.HostConfig.PortBindings[port+"/tcp"]
		if !hasLoopbackPortBinding(bindings, port) {
			incompatibilities = append(incompatibilities, "missing loopback host binding "+port+"->"+port)
		} else if hasNonLoopbackPortBinding(bindings, port) {
			incompatibilities = append(incompatibilities, fmt.Sprintf("port %s has non-loopback binding alongside the loopback one — SSH/HTTPS would be exposed beyond localhost", port))
		}
	}
	expectedMounts := []struct {
		source      string
		destination string
	}{
		{source: cacheDirs[0], destination: kindCICachePath},
		{source: cacheDirs[1], destination: kindReleaseCachePath},
	}
	for _, mount := range expectedMounts {
		if !hasBindMount(inspection.Mounts, mount.source, mount.destination) {
			incompatibilities = append(incompatibilities, fmt.Sprintf("missing bind mount %s->%s", mount.source, mount.destination))
		}
	}
	if len(incompatibilities) == 0 {
		return nil
	}

	return fmt.Errorf(
		"existing kind cluster %q is incompatible with the required macOS topology: %s; after confirming the cluster can be replaced, run `kind delete cluster --name %s`, then rerun oberth install (Oberth will not delete it automatically)",
		KindClusterName,
		strings.Join(incompatibilities, "; "),
		KindClusterName,
	)
}

func hasLoopbackPortBinding(bindings []dockerPortBinding, hostPort string) bool {
	for _, binding := range bindings {
		ip := net.ParseIP(binding.HostIP)
		if binding.HostPort == hostPort && ip != nil && ip.IsLoopback() {
			return true
		}
	}
	return false
}

// hasNonLoopbackPortBinding reports whether any binding for the given host port
// uses a non-loopback address (e.g. 0.0.0.0, a LAN IP, or an IPv6 wildcard).
// A port that binds both loopback and non-loopback exposes SSH/HTTPS beyond
// localhost, which is not the topology the installer creates.
func hasNonLoopbackPortBinding(bindings []dockerPortBinding, hostPort string) bool {
	for _, binding := range bindings {
		if binding.HostPort != hostPort {
			continue
		}
		ip := net.ParseIP(binding.HostIP)
		if ip == nil {
			// Unparseable or empty HostIP — treat as non-loopback.
			return true
		}
		if !ip.IsLoopback() {
			return true
		}
	}
	return false
}

func hasBindMount(mounts []dockerMount, source, destination string) bool {
	for _, mount := range mounts {
		if mount.Type == "bind" && filepath.Clean(mount.Source) == filepath.Clean(source) && filepath.Clean(mount.Destination) == filepath.Clean(destination) {
			return true
		}
	}
	return false
}

type oberthChartValues struct {
	Image struct {
		Ref string `json:"ref"`
	} `json:"image"`
}

// kindImagePlan describes how one chart image reaches the kind node.
//
// Digest-pinned refs cannot be loaded verbatim: the chart pins multi-arch
// OCI index digests, `docker pull repo@sha256:<index>` materializes only the
// current platform's child, and `docker save` of that partial pull emits an
// archive that still references the full index — `ctr images import
// --all-platforms` (what `kind load docker-image` runs) then walks the index
// and fails with "content digest ...: not found" on the missing sibling
// platform. The plan therefore pulls the platform child manifest, loads it
// under a deterministic local tag, and finally aliases the original digest
// ref inside the node's containerd so the digest-pinned pod spec resolves
// without registry access.
type kindImagePlan struct {
	chartRef string // exact ref from chart values; what the deployment references
	pullRef  string // ref docker-pulled on the host (platform child for digest refs)
	loadRef  string // ref loaded into kind (local tag for digest refs, chartRef otherwise)
	alias    bool   // alias chartRef to loadRef inside the node's containerd
}

func prepareKindImagesForCluster(ctx context.Context, cfg Config, deps Deps, clusterName string) error {
	if deps.RunHelm == nil {
		deps.RunHelm = DefaultRunHelm
	}
	if deps.RunCommand == nil {
		deps.RunCommand = DefaultRunCommand
	}
	if deps.Output == nil {
		deps.Output = io.Discard
	}

	if _, err := deps.RunHelm(ctx, []string{"repo", "add", "--force-update", oberthRepoName, OberthHelmRepoURL}); err != nil {
		return fmt.Errorf("add Oberth helm repo before resolving kind images: %w", err)
	}
	showArgs := oberthChartValuesArgs(cfg)
	valuesYAML, err := deps.RunHelm(ctx, showArgs)
	if err != nil {
		return fmt.Errorf("read Oberth chart values for kind image loading: %w", err)
	}
	var values oberthChartValues
	if err := yaml.Unmarshal(valuesYAML, &values); err != nil {
		return fmt.Errorf("parse Oberth chart values for kind image loading: %w", err)
	}
	// Job pods run repository-declared standard golang images pulled from the
	// public registry at run time; only the server image is preloaded.
	images := []string{strings.TrimSpace(values.Image.Ref)}
	if images[0] == "" {
		return errors.New("oberth chart values must define image.ref for kind image loading")
	}

	arch := ""
	plans := make([]kindImagePlan, 0, len(images))
	for _, image := range images {
		plan := kindImagePlan{chartRef: image, pullRef: image, loadRef: image}
		_, _, isDigestRef := splitImageDigestRef(image)
		if isDigestRef {
			plan = planKindDigestImage(image)
		}
		_, cacheErr := deps.RunCommand(ctx, nil, "docker", "image", "inspect", plan.loadRef)
		if cacheErr == nil {
			_, _ = fmt.Fprintf(deps.Output, "%s already exists locally; skipping pull.\n", plan.loadRef)
		} else {
			if isDigestRef {
				if arch == "" {
					arch = kindNodePlatformArch(ctx, deps)
				}
				childRef, err := resolvePlatformManifestRef(ctx, deps, image, arch)
				if err != nil {
					return err
				}
				if childRef != "" {
					plan.pullRef = childRef
				}
			}
			_, _ = fmt.Fprintf(deps.Output, "Pulling %s for kind...\n", plan.pullRef)
			output, err := deps.RunCommand(ctx, nil, "docker", "pull", plan.pullRef)
			if err != nil {
				return fmt.Errorf("pull Oberth image %s: %w%s\nAuthenticate Docker to GAR (for example, `gcloud auth configure-docker europe-west4-docker.pkg.dev`) or configure equivalent Docker registry credentials, then rerun oberth install", plan.pullRef, err, commandOutputSuffix(output))
			}
			if plan.loadRef != plan.pullRef {
				if output, err := deps.RunCommand(ctx, nil, "docker", "tag", plan.pullRef, plan.loadRef); err != nil {
					return fmt.Errorf("tag Oberth image %s as %s for kind loading: %w%s", plan.pullRef, plan.loadRef, err, commandOutputSuffix(output))
				}
			}
		}
		// ALWAYS (re)load into the kind node — even on a host-cache hit. The
		// host Docker cache and the node's containerd have independent
		// lifetimes: the cluster may have been deleted and recreated since the
		// image was pulled, or a prior load may have failed after its pull
		// succeeded. Host presence therefore never proves node presence, and
		// skipping the load on a cache hit wedges reruns (the digest alias
		// step fails with "not found" against a fresh node). Loading from the
		// local daemon is cheap; only the network pull is worth skipping.
		// Loading each image before preparing the next also keeps mid-run
		// retries safe: if a later pull fails, everything already pulled is
		// already in the node.
		loadArgs := []string{"load", "docker-image", "--name", clusterName, plan.loadRef}
		if output, err := deps.RunCommand(ctx, nil, "kind", loadArgs...); err != nil {
			return fmt.Errorf("load Oberth image %s into kind cluster %q: %w%s", plan.loadRef, clusterName, err, commandOutputSuffix(output))
		}
		plans = append(plans, plan)
	}

	return aliasKindDigestImages(ctx, deps, clusterName, plans)
}

// planKindDigestImage derives the load-and-alias plan for one digest-pinned
// chart ref. The local tag is deterministic per exact chart ref, so repeated
// installs reuse it and different releases never collide.
func planKindDigestImage(chartRef string) kindImagePlan {
	refHash := sha256.Sum256([]byte(chartRef))
	localTag := imageRepository(chartRef) + ":oberth-kind-" + hex.EncodeToString(refHash[:])[:12]
	return kindImagePlan{
		chartRef: chartRef,
		pullRef:  chartRef,
		loadRef:  localTag,
		alias:    true,
	}
}

// aliasKindDigestImages creates the digest-named image records inside every
// kind node's containerd so pods referencing the chart's immutable digests
// start from the loaded content instead of attempting an unauthenticated
// registry pull.
func aliasKindDigestImages(ctx context.Context, deps Deps, clusterName string, plans []kindImagePlan) error {
	aliases := make([]kindImagePlan, 0, len(plans))
	for _, plan := range plans {
		if plan.alias {
			aliases = append(aliases, plan)
		}
	}
	if len(aliases) == 0 {
		return nil
	}
	output, err := deps.RunCommand(ctx, nil, "kind", "get", "nodes", "--name", clusterName)
	if err != nil {
		return fmt.Errorf("list kind nodes for cluster %q: %w%s", clusterName, err, commandOutputSuffix(output))
	}
	nodes := strings.Fields(string(output))
	if len(nodes) == 0 {
		return fmt.Errorf("kind cluster %q reported no nodes for image digest aliasing", clusterName)
	}
	for _, node := range nodes {
		for _, plan := range aliases {
			_, _ = fmt.Fprintf(deps.Output, "Aliasing %s on kind node %s...\n", plan.chartRef, node)
			tagArgs := []string{"exec", node, "ctr", "--namespace=k8s.io", "images", "tag", "--force", plan.loadRef, plan.chartRef}
			if output, err := deps.RunCommand(ctx, nil, "docker", tagArgs...); err != nil {
				return fmt.Errorf("alias Oberth image digest %s on kind node %s: %w%s", plan.chartRef, node, err, commandOutputSuffix(output))
			}
		}
	}
	return nil
}

// registryManifestEntry is one child of an OCI image index / Docker manifest
// list as printed by `docker manifest inspect`.
type registryManifestEntry struct {
	Digest   string `json:"digest"`
	Platform struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform"`
}

type registryManifestIndex struct {
	Manifests []registryManifestEntry `json:"manifests"`
}

// resolvePlatformManifestRef resolves a digest-pinned multi-arch ref to its
// linux/<arch> child manifest ref via `docker manifest inspect`. It returns
// "" (pull the original ref unchanged) when the ref is already a
// single-platform manifest or when inspection is unavailable — never worse
// than the previous behavior — and errors only when the index definitively
// lacks the required platform.
func resolvePlatformManifestRef(ctx context.Context, deps Deps, ref, arch string) (string, error) {
	output, err := deps.RunCommand(ctx, nil, "docker", "manifest", "inspect", ref)
	if err != nil {
		return "", nil
	}
	var index registryManifestIndex
	if err := json.Unmarshal(output, &index); err != nil {
		return "", nil
	}
	if len(index.Manifests) == 0 {
		return "", nil
	}
	available := make([]string, 0, len(index.Manifests))
	for _, entry := range index.Manifests {
		if entry.Platform.OS == "" && entry.Platform.Architecture == "" {
			continue
		}
		platform := entry.Platform.OS + "/" + entry.Platform.Architecture
		if entry.Platform.OS == "linux" && entry.Platform.Architecture == arch && isSHA256Digest(entry.Digest) {
			return imageRepository(ref) + "@" + entry.Digest, nil
		}
		if platform != "unknown/unknown" {
			available = append(available, platform)
		}
	}
	return "", fmt.Errorf("image %s has no linux/%s variant (available: %s); the pinned chart images cannot run on this kind node platform", ref, arch, strings.Join(available, ", "))
}

// kindNodePlatformArch reports the architecture kind nodes will run on: the
// docker server's architecture (authoritative for Docker Desktop / OrbStack
// VMs), falling back to this binary's architecture.
func kindNodePlatformArch(ctx context.Context, deps Deps) string {
	output, err := deps.RunCommand(ctx, nil, "docker", "version", "--format", "{{.Server.Arch}}")
	if err == nil {
		arch := strings.TrimSpace(string(output))
		if arch != "" && !strings.ContainsAny(arch, " \t\n") {
			return arch
		}
	}
	return runtime.GOARCH
}

// splitImageDigestRef splits repo[@sha256:digest] and reports whether the ref
// is digest-pinned.
func splitImageDigestRef(ref string) (repo, digest string, ok bool) {
	repo, digest, found := strings.Cut(ref, "@")
	if !found || !strings.HasPrefix(digest, "sha256:") {
		return ref, "", false
	}
	return repo, digest, true
}

// imageRepository strips any digest and tag from an image ref, preserving
// registry host:port components.
func imageRepository(ref string) string {
	repo, _, _ := strings.Cut(ref, "@")
	slash := strings.LastIndex(repo, "/")
	if colon := strings.LastIndex(repo, ":"); colon > slash {
		repo = repo[:colon]
	}
	return repo
}

func isSHA256Digest(digest string) bool {
	rest, found := strings.CutPrefix(digest, "sha256:")
	if !found || len(rest) != 64 {
		return false
	}
	for _, r := range rest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func oberthChartValuesArgs(cfg Config) []string {
	// --chart means this deployment's chart is on disk, so its values are read
	// from there. Reading the published chart instead makes --chart a half
	// promise, and fails outright when the binary's version is not a published
	// chart version, which is true of every fork build.
	if local := strings.TrimSpace(cfg.ChartPath); local != "" {
		return []string{"show", "values", local}
	}
	args := []string{"show", "values", oberthRepoName + "/oberth"}
	if cfg.ChartVersion != "" {
		args = append(args, "--version", cfg.ChartVersion)
	}
	return args
}

func kindNameFromContext(contextName string) (string, bool) {
	name, found := strings.CutPrefix(contextName, "kind-")
	return name, found && name != ""
}

func commandOutputSuffix(output []byte) string {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return ""
	}
	return ": " + text
}

// DefaultRunCommand runs a host command without a shell and returns its
// combined output for actionable prerequisite and registry errors.
func DefaultRunCommand(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
	// #nosec G204 -- callers select a fixed executable and pass structured
	// arguments; no shell interpolation occurs.
	command := exec.CommandContext(ctx, name, args...)
	command.Env = subprocessEnvironment()
	if len(input) > 0 {
		command.Stdin = bytes.NewReader(input)
	}
	return command.CombinedOutput()
}

// DefaultRunInteractive runs a host command wired to the operator's own
// terminal, so pod-side interactive flows (kubectl exec -i ... oberth
// upstream add) can prompt, stream progress, and read answers directly.
func DefaultRunInteractive(ctx context.Context, name string, args ...string) error {
	// #nosec G204 -- callers select a fixed executable and pass structured
	// arguments; no shell interpolation occurs.
	command := exec.CommandContext(ctx, name, args...)
	command.Env = subprocessEnvironment()
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

// StdinIsTerminal reports whether stdin is an interactive terminal. The
// isatty check (not a char-device stat, which /dev/null would pass) keeps
// piped and CI runs on the printed manual-steps path.
func StdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}
