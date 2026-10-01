package installer

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

const watchAdoptionSchema = "oberth.watch-adoption/v1"

// Adoption values are public connector inputs, not authority to change other
// installed services or their security policy.
func readWatchValues(files []string) ([]byte, error) {
	return readWatchValuesMode(files, false)
}

func readWatchValuesMode(files []string, recovery bool) ([]byte, error) {
	if len(files) > 16 {
		return nil, errors.New("too many public watch values files")
	}
	merged := map[string]map[string]any{}
	for _, name := range files {
		f, err := os.Open(name) // #nosec G304 -- explicit administrator values path; bounded public whitelist, sanitized errors, no raw output.
		if err != nil {
			return nil, errors.New("cannot open public watch values")
		}
		raw, err := io.ReadAll(io.LimitReader(f, 262145))
		_ = f.Close()
		if err != nil || len(raw) > 262144 {
			return nil, errors.New("public watch values exceed bound")
		}
		var values map[string]map[string]any
		if sigsyaml.UnmarshalStrict(raw, &values) != nil {
			return nil, errors.New("public watch values must contain only connector enablement and public trust")
		}
		for section, fields := range values {
			if merged[section] == nil {
				merged[section] = map[string]any{}
			}
			if recovery && (section == "compatibility" || section == "argo") {
				if err := mergeWatchRecoveryValues(merged[section], fields, section); err != nil {
					return nil, err
				}
				continue
			}
			if fields == nil || (section != "watchTunnel" && section != "secretstore") {
				return nil, errors.New("public watch values contain an unrelated setting")
			}
			for key, value := range fields {
				merged[section][key] = value
				if section == "watchTunnel" && key == "enabled" {
					if enabled, ok := value.(bool); !ok || !enabled {
						return nil, errors.New("watch adoption requires boolean enabled=true")
					}
					continue
				}
				cert, ok := value.(string)
				approvedCA := (section == "watchTunnel" && key == "originCACert") || (section == "secretstore" && key == "caCert")
				if !ok || !approvedCA || !watchPublicCA(map[string]string{"ca.crt": cert}) {
					return nil, errors.New("public watch values contain an unapproved input or invalid public certificate")
				}
			}
		}
	}
	return json.Marshal(merged)
}

func freezeWatchValues(cfg Config) (Config, func(), error) {
	if cfg.WatchAdoptionPlan == "" {
		return cfg, func() {}, nil
	}
	raw, err := readWatchConfiguredValues(cfg)
	if err != nil {
		return cfg, func() {}, err
	}
	cfg.watchValuesSHA256 = watchValuesDigest(raw)
	f, err := sealedWatchValues(raw)
	if err != nil {
		return cfg, func() {}, err
	}
	cfg.ValuesFiles = []string{fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), f.Fd())}
	return cfg, func() { _ = f.Close() }, nil
}

func isWatchPreview(args []string) bool {
	flags := map[string]bool{}
	for _, arg := range args {
		flags[arg] = true
	}
	return flags["upgrade"] && flags["--dry-run=server"] && flags["--no-hooks"] && flags["--take-ownership"] && flags["--output=json"]
}

type watchPreviewBuffer struct {
	buffer bytes.Buffer
	limit  int
	cancel context.CancelFunc
}

func (b *watchPreviewBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		b.cancel()
		return 0, errors.New("watch preview output exceeds bound")
	}
	return b.buffer.Write(p)
}

func (b *watchPreviewBuffer) Bytes() []byte { return b.buffer.Bytes() }

// The mixed Helm release envelope stays bounded in process memory. Neither
// errors nor output expose its values, manifests, or stderr to the caller.
func runWatchPreview(ctx context.Context, args []string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdout := &watchPreviewBuffer{limit: 8 << 20, cancel: cancel}
	stderr := &watchPreviewBuffer{limit: 64 << 10, cancel: cancel}
	defer func() { clear(stderr.Bytes()) }()
	cmd := exec.CommandContext(ctx, "helm", args...) // #nosec G204 -- fixed executable and installer-constructed preview arguments.
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		clear(stdout.Bytes())
		return nil, errors.New("watch adoption Helm preview failed or exceeded output bound")
	}
	return stdout.Bytes(), nil
}

type watchOwnership struct {
	ManagedBy string `json:"managed_by"`
	Release   string `json:"release"`
	Namespace string `json:"namespace"`
}
type watchAdoptionObject struct {
	Kind            string          `json:"kind"`
	Name            string          `json:"name"`
	UID             string          `json:"uid"`
	ResourceVersion string          `json:"resource_version"`
	Ownership       watchOwnership  `json:"ownership"`
	Spec            json.RawMessage `json:"spec"`
	TargetSpec      json.RawMessage `json:"target_spec"`
}
type watchAdoptionPlan struct {
	Schema          string                `json:"schema"`
	Namespace       string                `json:"namespace"`
	ChartVersion    string                `json:"chart_version"`
	ReleaseRevision int64                 `json:"release_revision"`
	ExpiresAt       time.Time             `json:"expires_at"`
	Objects         []watchAdoptionObject `json:"objects"`
}
type watchObserved struct {
	meta metav1.Object
	spec json.RawMessage
}
type watchAdopted struct {
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resource_version"`
}

var watchObjects = map[string]string{"ServiceAccount": "cloudflared-watch", "Deployment": "cloudflared-watch-oberth-v2"}

func watchObjectAllowed(kind, name string) bool {
	return watchObjects[kind] == name && name != "" || kind == "ConfigMap" && (name == "cloudflared-watch-openbao-ca" || name == "cloudflared-watch-oberth-origin-ca")
}
func watchPublicCA(data map[string]string) bool {
	if len(data) != 1 || len(data["ca.crt"]) == 0 || len(data["ca.crt"]) > 65536 {
		return false
	}
	rest := []byte(strings.TrimSpace(data["ca.crt"]))
	count := 0
	for len(rest) > 0 {
		if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
			return false
		}
		end := bytes.Index(rest, []byte("-----END CERTIFICATE-----"))
		if end < 0 {
			return false
		}
		end += len("-----END CERTIFICATE-----")
		if bytes.Count(rest[:end], []byte("-----BEGIN")) != 1 {
			return false
		}
		block, remainder := pem.Decode(rest[:end])
		if block == nil || len(bytes.TrimSpace(remainder)) != 0 || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return false
		}
		// A configured trust anchor can be the origin's self-signed leaf;
		// Oberth's existing generated TLS certificate deliberately is not a CA.
		_, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return false
		}
		count++
		rest = bytes.TrimSpace(rest[end:])
	}
	return count > 0
}
func watchSpec(object any) (watchObserved, error) {
	var meta metav1.Object
	var spec any
	switch o := object.(type) {
	case *appsv1.Deployment:
		meta = o
		spec = o.Spec
		if o.Spec.Template.Spec.ServiceAccountName != "cloudflared-watch" || len(o.Spec.Template.Spec.Containers) != 1 || len(o.Spec.Template.Spec.InitContainers) != 1 {
			return watchObserved{}, errors.New("connector deployment shape differs")
		}
		for _, c := range append(o.Spec.Template.Spec.InitContainers, o.Spec.Template.Spec.Containers...) {
			for _, e := range c.Env {
				if e.ValueFrom != nil || (e.Name != "BAO_ADDR" && e.Name != "BAO_CACERT" && e.Name != "BAO_CLIENT_TIMEOUT" && e.Name != "HOME") {
					return watchObserved{}, errors.New("connector contains unapproved environment input")
				}
			}
			if len(c.EnvFrom) != 0 {
				return watchObserved{}, errors.New("connector contains environment source")
			}
		}
	case *corev1.ServiceAccount:
		meta = o
		if len(o.Secrets) != 0 || len(o.ImagePullSecrets) != 0 {
			return watchObserved{}, errors.New("connector service account contains credential references")
		}
		spec = struct {
			Automount *bool `json:"automountServiceAccountToken"`
		}{o.AutomountServiceAccountToken}
	case *corev1.ConfigMap:
		meta = o
		if len(o.BinaryData) != 0 || !watchPublicCA(o.Data) {
			return watchObserved{}, errors.New("connector CA must contain only public CA certificates")
		}
		spec = struct {
			Data      map[string]string `json:"data"`
			Immutable *bool             `json:"immutable,omitempty"`
		}{o.Data, o.Immutable}
	default:
		return watchObserved{}, errors.New("unsupported connector object")
	}
	b, err := json.Marshal(spec)
	return watchObserved{meta: meta, spec: b}, err
}
func watchOwner(meta metav1.Object) watchOwnership {
	return watchOwnership{meta.GetLabels()["app.kubernetes.io/managed-by"], meta.GetAnnotations()["meta.helm.sh/release-name"], meta.GetAnnotations()["meta.helm.sh/release-namespace"]}
}
func sameWatchJSON(a, b []byte) bool {
	var x, y any
	da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	if da.Decode(&x) != nil || db.Decode(&y) != nil || !errors.Is(da.Decode(new(any)), io.EOF) || !errors.Is(db.Decode(new(any)), io.EOF) {
		return false
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(ax, by)
}
func readWatchPlan(cfg Config) (watchAdoptionPlan, error) {
	var p watchAdoptionPlan
	f, err := os.Open(cfg.WatchAdoptionPlan)
	if err != nil {
		return p, errors.New("cannot open public watch adoption plan")
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 262145))
	if err != nil || len(raw) > 262144 {
		return p, errors.New("public watch adoption plan exceeds bound")
	}
	if !uniqueWatchJSON(raw) || !exactWatchFields(raw) {
		return p, errors.New("public watch adoption plan contains duplicate or invalid JSON fields")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
		return p, errors.New("invalid public watch adoption plan")
	}
	if p.Schema != watchAdoptionSchema || p.Namespace != cfg.Namespace || p.ChartVersion != cfg.ChartVersion || p.ReleaseRevision < 1 || len(p.Objects) != 4 || !time.Now().Before(p.ExpiresAt) || time.Until(p.ExpiresAt) > 24*time.Hour {
		return p, errors.New("watch adoption plan scope, version or expiry differs")
	}
	seen := map[string]bool{}
	rv := regexp.MustCompile(`^[1-9][0-9]*$`)
	for _, o := range p.Objects {
		key := o.Kind + "/" + o.Name
		if !watchObjectAllowed(o.Kind, o.Name) || seen[key] || o.UID == "" || !rv.MatchString(o.ResourceVersion) || !json.Valid(o.Spec) || !json.Valid(o.TargetSpec) {
			return p, errors.New("watch adoption object identity invalid")
		}
		seen[key] = true
		if o.Ownership != (watchOwnership{}) && o.Ownership != (watchOwnership{"Helm", "oberth", cfg.Namespace}) {
			return p, errors.New("watch adoption refuses foreign or partial Helm ownership")
		}
	}
	return p, nil
}

// encoding/json's case-insensitive field matching is unsuitable for an
// explicitly reviewed authority plan. Require the exact public spelling and
// non-null presence of every typed field before decoding it.
func exactWatchFields(raw []byte) bool {
	object := func(raw []byte, names ...string) (map[string]json.RawMessage, bool) {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || len(fields) != len(names) {
			return nil, false
		}
		for _, name := range names {
			v, ok := fields[name]
			if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return nil, false
			}
		}
		return fields, true
	}
	p, ok := object(raw, "schema", "namespace", "chart_version", "release_revision", "expires_at", "objects")
	if !ok {
		return false
	}
	var rows []json.RawMessage
	if json.Unmarshal(p["objects"], &rows) != nil {
		return false
	}
	for _, row := range rows {
		o, valid := object(row, "kind", "name", "uid", "resource_version", "ownership", "spec", "target_spec")
		if !valid {
			return false
		}
		if _, valid = object(o["ownership"], "managed_by", "release", "namespace"); !valid {
			return false
		}
	}
	return true
}

func requireWatchRevision(ctx context.Context, deps Deps, p watchAdoptionPlan) error {
	if !time.Now().Before(p.ExpiresAt) {
		return errors.New("watch adoption plan expired before the next effect")
	}
	release, exists := findHelmRelease(ctx, deps, "oberth", p.Namespace)
	revision, err := strconv.ParseInt(strings.Trim(string(release.Revision), `"`), 10, 64)
	if !exists || err != nil || revision != p.ReleaseRevision || release.Namespace != p.Namespace || release.Status != "deployed" {
		return errors.New("watch adoption requires the reviewed existing deployed Oberth release revision")
	}
	if !time.Now().Before(p.ExpiresAt) {
		return errors.New("watch adoption plan expired during release revision observation")
	}
	return nil
}
func getWatchObject(ctx context.Context, deps Deps, ns string, o watchAdoptionObject) (watchObserved, error) {
	switch o.Kind {
	case "Deployment":
		v, e := deps.KubeClient.AppsV1().Deployments(ns).Get(ctx, o.Name, metav1.GetOptions{})
		if e != nil {
			return watchObserved{}, e
		}
		return watchSpec(v)
	case "ServiceAccount":
		v, e := deps.KubeClient.CoreV1().ServiceAccounts(ns).Get(ctx, o.Name, metav1.GetOptions{})
		if e != nil {
			return watchObserved{}, e
		}
		return watchSpec(v)
	case "ConfigMap":
		v, e := deps.KubeClient.CoreV1().ConfigMaps(ns).Get(ctx, o.Name, metav1.GetOptions{})
		if e != nil {
			return watchObserved{}, e
		}
		return watchSpec(v)
	}
	return watchObserved{}, errors.New("unsupported connector object")
}
func preflightWatchObjects(ctx context.Context, deps Deps, p watchAdoptionPlan) ([]watchObserved, error) {
	observations := make([]watchObserved, 0, 4)
	for _, o := range p.Objects {
		v, e := getWatchObject(ctx, deps, p.Namespace, o)
		if e != nil || v.meta.GetNamespace() != p.Namespace || string(v.meta.GetUID()) != o.UID || v.meta.GetResourceVersion() != o.ResourceVersion || v.meta.GetDeletionTimestamp() != nil || watchOwner(v.meta) != o.Ownership || !sameWatchJSON(v.spec, o.Spec) {
			return nil, fmt.Errorf("watch adoption object state differs for %s/%s", o.Kind, o.Name)
		}
		observations = append(observations, v)
	}
	return observations, nil
}
func validateWatchTarget(ctx context.Context, cfg Config, deps Deps, p watchAdoptionPlan) error {
	// Run Helm's real upgrade calculation, including --reuse-values and all
	// installer overrides. A template render coalesces new defaults differently.
	// Ownership bypass exists only in this nonmutating preview; the actual
	// upgrade uses the ordinary builder without either preview flag.
	args := append(OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{}), "--dry-run=server", "--output=json", "--no-hooks", "--take-ownership")
	raw, err := deps.RunHelm(ctx, args)
	defer clear(raw)
	if err != nil || len(raw) > 8<<20 {
		return errors.New("watch adoption target render failed")
	}
	var preview struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Version   int64  `json:"version"`
		Manifest  string `json:"manifest"`
		Chart     struct {
			Metadata struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"metadata"`
		} `json:"chart"`
	}
	if json.Unmarshal(raw, &preview) != nil || preview.Name != "oberth" || preview.Namespace != cfg.Namespace || preview.Version != p.ReleaseRevision+1 || preview.Manifest == "" || preview.Chart.Metadata.Name != "oberth" || canonicalChartVersion(preview.Chart.Metadata.Version) != canonicalChartVersion(p.ChartVersion) {
		return errors.New("watch adoption upgrade preview identity or revision differs")
	}
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(preview.Manifest), 4096)
	found := map[string]watchObserved{}
	for {
		var body json.RawMessage
		e := decoder.Decode(&body)
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return errors.New("watch adoption target render invalid")
		}
		if len(body) == 0 || bytes.Equal(body, []byte("null")) {
			continue
		}
		var id struct {
			Kind     string            `json:"kind"`
			Metadata metav1.ObjectMeta `json:"metadata"`
		}
		if json.Unmarshal(body, &id) != nil {
			return errors.New("watch adoption target identity invalid")
		}
		if !watchObjectAllowed(id.Kind, id.Metadata.Name) {
			continue
		}
		if id.Metadata.Namespace != cfg.Namespace {
			return errors.New("watch adoption target namespace differs")
		}
		key := id.Kind + "/" + id.Metadata.Name
		if _, ok := found[key]; ok {
			return errors.New("duplicate watch adoption target")
		}
		var object any
		switch id.Kind {
		case "Deployment":
			object = &appsv1.Deployment{}
		case "ServiceAccount":
			object = &corev1.ServiceAccount{}
		case "ConfigMap":
			object = &corev1.ConfigMap{}
		}
		if json.Unmarshal(body, object) != nil {
			return errors.New("watch adoption target object invalid")
		}
		v, e := watchSpec(object)
		if e != nil {
			return e
		}
		found[key] = v
	}
	for _, o := range p.Objects {
		v, ok := found[o.Kind+"/"+o.Name]
		if !ok || !sameWatchJSON(v.spec, o.TargetSpec) {
			return errors.New("watch adoption target differs from reviewed plan")
		}
	}
	return nil
}
func patchWatchObject(ctx context.Context, deps Deps, ns string, o watchAdoptionObject, v watchObserved) (watchObserved, error) {
	labels := map[string]string{}
	for k, x := range v.meta.GetLabels() {
		labels[k] = x
	}
	labels["app.kubernetes.io/managed-by"] = "Helm"
	annotations := map[string]string{}
	for k, x := range v.meta.GetAnnotations() {
		annotations[k] = x
	}
	annotations["meta.helm.sh/release-name"] = "oberth"
	annotations["meta.helm.sh/release-namespace"] = ns
	patch, _ := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": o.UID}, {"op": "test", "path": "/metadata/resourceVersion", "value": o.ResourceVersion}, {"op": "add", "path": "/metadata/labels", "value": labels}, {"op": "add", "path": "/metadata/annotations", "value": annotations}})
	opts := metav1.PatchOptions{FieldManager: "oberth-watch-adoption"}
	switch o.Kind {
	case "Deployment":
		x, e := deps.KubeClient.AppsV1().Deployments(ns).Patch(ctx, o.Name, types.JSONPatchType, patch, opts)
		if e != nil {
			return watchObserved{}, e
		}
		return watchSpec(x)
	case "ServiceAccount":
		x, e := deps.KubeClient.CoreV1().ServiceAccounts(ns).Patch(ctx, o.Name, types.JSONPatchType, patch, opts)
		if e != nil {
			return watchObserved{}, e
		}
		return watchSpec(x)
	case "ConfigMap":
		x, e := deps.KubeClient.CoreV1().ConfigMaps(ns).Patch(ctx, o.Name, types.JSONPatchType, patch, opts)
		if e != nil {
			return watchObserved{}, e
		}
		return watchSpec(x)
	}
	return watchObserved{}, errors.New("unsupported connector object")
}

// AI-CONTRACT: The explicit, expiring public plan authorizes metadata adoption
// of only four named connector objects. Preflight the full set and exact target
// before the first CAS. Never delete/recreate, retry or roll back partial work.
func adoptWatchTunnel(ctx context.Context, cfg Config, deps Deps, dryRun bool) error {
	_, err := adoptWatchTunnelReceipt(ctx, cfg, deps, dryRun)
	return err
}

func adoptWatchTunnelReceipt(ctx context.Context, cfg Config, deps Deps, dryRun bool) ([]watchAdopted, error) {
	if cfg.WatchAdoptionPlan == "" {
		return nil, nil
	}
	if cfg.watchRecovery != nil {
		return executeWatchRecovery(ctx, cfg, deps, dryRun)
	}
	if p, err := readWatchRecoveryPlan(cfg); err != nil {
		return nil, err
	} else if p != nil {
		return nil, errors.New("v2 watch recovery requires prepared signed artifacts")
	}
	p, e := readWatchPlan(cfg)
	if e != nil {
		return nil, e
	}
	if e = requireWatchRevision(ctx, deps, p); e != nil {
		return nil, e
	}
	observed, e := preflightWatchObjects(ctx, deps, p)
	if e != nil {
		return nil, e
	}
	if e = validateWatchTarget(ctx, cfg, deps, p); e != nil {
		return nil, e
	}
	if e = requireWatchRevision(ctx, deps, p); e != nil {
		return nil, e
	}
	if dryRun {
		return nil, nil
	}
	committed := []watchAdopted{}
	for i, o := range p.Objects {
		if e = requireWatchRevision(ctx, deps, p); e != nil {
			return committed, e
		}
		if o.Ownership == (watchOwnership{"Helm", "oberth", p.Namespace}) {
			continue // An explicitly reviewed partial-forward-repair plan may retain prior ownership.
		}
		v, err := patchWatchObject(ctx, deps, p.Namespace, o, observed[i])
		if err != nil || v.meta.GetNamespace() != p.Namespace || string(v.meta.GetUID()) != o.UID || v.meta.GetResourceVersion() == "" || v.meta.GetResourceVersion() == o.ResourceVersion || watchOwner(v.meta) != (watchOwnership{"Helm", "oberth", p.Namespace}) || !sameWatchJSON(v.spec, o.Spec) {
			b, _ := json.Marshal(committed)
			return committed, fmt.Errorf("watch adoption stopped; confirmed metadata commits=%s; outcome uncertain for %s/%s; retain state and prepare a fresh reviewed plan, no automatic retry", b, o.Kind, o.Name)
		}
		committed = append(committed, watchAdopted{o.Kind, o.Name, o.UID, v.meta.GetResourceVersion()})
		p.Objects[i].ResourceVersion = v.meta.GetResourceVersion()
		p.Objects[i].Ownership = watchOwnership{"Helm", "oberth", p.Namespace}
	}
	if _, e = preflightWatchObjects(ctx, deps, p); e != nil {
		return committed, e
	}
	if e = requireWatchRevision(ctx, deps, p); e != nil {
		return committed, e
	}
	b, _ := json.Marshal(committed)
	if deps.Output != nil {
		_, _ = fmt.Fprintf(deps.Output, "Watch connector metadata adopted: %s\n", b)
	}
	return committed, nil
}

// Reject ambiguity before typed decoding, including duplicate fields inside
// reviewed old/target specifications. Bound nesting independently of file size.
func uniqueWatchJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var read func(int) bool
	read = func(depth int) bool {
		if depth > 64 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				name, ok := key.(string)
				if e != nil || !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !read(depth + 1) {
					return false
				}
			}
			last, e := d.Token()
			return e == nil && last == json.Delim('}')
		case '[':
			for d.More() {
				if !read(depth + 1) {
					return false
				}
			}
			last, e := d.Token()
			return e == nil && last == json.Delim(']')
		default:
			return false
		}
	}
	if !read(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}
