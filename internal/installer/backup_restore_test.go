package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

const (
	testBackupToken = "s.backup-root-token"
	// testSecretValue must never appear in any argv.
	testSecretValue = "upstream-private-key-canary"
)

var sourceCompat = backupCompatibility{
	SchemaIdentity: "acme-oberth-schema-v1",
	AuditDomain:    "acme-oberth-audit-v1",
	WitnessKeyInfo: "acme-oberth-audit-witness-v1",
}

var defaultCompat = backupCompatibility{
	SchemaIdentity: "oberth-schema-v1",
	AuditDomain:    "oberth-audit-v1",
	WitnessKeyInfo: "oberth-audit-witness-v1",
}

// --- A stateful in-memory OpenBao behind kubectl exec ---

type fakeOpenBao struct {
	t        *testing.T
	kv       map[string]map[string]any
	policies map[string]string
	roles    map[string]map[string]any
	// dropOnRoleWrite simulates a lossy writer that loses one field.
	dropOnRoleWrite string
	writes          []string
}

func newFakeOpenBao(t *testing.T) *fakeOpenBao {
	return &fakeOpenBao{t: t, kv: map[string]map[string]any{}, policies: map[string]string{}, roles: map[string]map[string]any{}}
}

func (f *fakeOpenBao) handle(command []string, input []byte) ([]byte, error) {
	f.t.Helper()
	if command[0] == "bao" {
		if strings.Join(command[1:], " ") == "status -format=json" {
			return []byte(`{"initialized":true,"sealed":false,"storage_type":"raft"}`), nil
		}
		f.t.Fatalf("unauthenticated bao command %v", command)
	}
	if len(command) < 4 || command[0] != "sh" || command[2] != tokenShellPreamble || command[3] != "bao" {
		f.t.Fatalf("bao command without the token-on-stdin wrapper: %v", command)
	}
	token, body, _ := bytes.Cut(input, []byte("\n"))
	if string(token) != testBackupToken {
		f.t.Fatalf("first stdin line is not the root token")
	}
	args := command[4:]
	notFound := func(path string) ([]byte, error) {
		return []byte("No value found at " + path), errors.New("exit status 2")
	}
	reply := func(value any) ([]byte, error) {
		out, err := json.Marshal(value)
		if err != nil {
			f.t.Fatal(err)
		}
		return out, nil
	}
	switch {
	case args[0] == "list":
		path := args[2]
		var keys []string
		switch {
		case strings.HasPrefix(path, "oberth/metadata/"):
			prefix := strings.TrimPrefix(path, "oberth/metadata/")
			seen := map[string]bool{}
			for p := range f.kv {
				if rest, ok := strings.CutPrefix(p, prefix); ok {
					if head, _, nested := strings.Cut(rest, "/"); nested {
						seen[head+"/"] = true
					} else {
						seen[rest] = true
					}
				}
			}
			for key := range seen {
				keys = append(keys, key)
			}
		case path == "auth/kubernetes/role":
			for name := range f.roles {
				keys = append(keys, name)
			}
		case path == "oberth-transit/keys":
			keys = []string{"trusted-plan-artifacts"}
		}
		if len(keys) == 0 {
			return notFound(path)
		}
		sort.Strings(keys)
		return reply(keys)
	case args[0] == "read":
		path := args[2]
		switch {
		case strings.HasPrefix(path, "oberth/data/"):
			data, ok := f.kv[strings.TrimPrefix(path, "oberth/data/")]
			if !ok {
				return notFound(path)
			}
			return reply(map[string]any{"data": map[string]any{"data": data, "metadata": map[string]any{"version": 1}}})
		case strings.HasPrefix(path, "auth/kubernetes/role/"):
			role, ok := f.roles[strings.TrimPrefix(path, "auth/kubernetes/role/")]
			if !ok {
				return notFound(path)
			}
			return reply(map[string]any{"data": role})
		case path == "auth/kubernetes/config":
			return reply(map[string]any{"data": map[string]any{"kubernetes_host": kubernetesHostInCluster}})
		case path == "oberth-transit/keys/trusted-plan-artifacts":
			return reply(map[string]any{"data": map[string]any{"type": "aes256-gcm96", "exportable": false}})
		case path == "oberth-pki/cert/ca":
			return reply(map[string]any{"data": map[string]any{"certificate": "-----BEGIN CERTIFICATE-----\nold-ca\n-----END CERTIFICATE-----"}})
		}
		return notFound(path)
	case args[0] == "write":
		path := args[1]
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			f.t.Fatalf("write %s: stdin is not JSON: %v", path, err)
		}
		f.writes = append(f.writes, path)
		switch {
		case strings.HasPrefix(path, "oberth/data/"):
			data, _ := payload["data"].(map[string]any)
			f.kv[strings.TrimPrefix(path, "oberth/data/")] = data
		case strings.HasPrefix(path, "auth/kubernetes/role/"):
			for alias, successor := range deprecatedRoleAliases {
				if _, both := payload[alias]; both {
					if _, ok := payload[successor]; ok {
						return []byte(fmt.Sprintf("Cannot specify both %q and %q", alias, successor)), errors.New("exit status 2")
					}
				}
			}
			delete(payload, f.dropOnRoleWrite)
			f.roles[strings.TrimPrefix(path, "auth/kubernetes/role/")] = payload
		default:
			f.t.Fatalf("unexpected bao write %s", path)
		}
		return nil, nil
	case args[0] == "policy" && args[1] == "list":
		names := []string{"default", "root"}
		for name := range f.policies {
			names = append(names, name)
		}
		sort.Strings(names)
		return []byte(strings.Join(names, "\n") + "\n"), nil
	case args[0] == "policy" && args[1] == "read":
		rules, ok := f.policies[args[2]]
		if !ok {
			return []byte("No policy named: " + args[2]), errors.New("exit status 2")
		}
		return []byte(rules + "\n"), nil
	case args[0] == "policy" && args[1] == "write":
		f.writes = append(f.writes, "policy/"+args[2])
		f.policies[args[2]] = string(body)
		return nil, nil
	case args[0] == "auth" && args[1] == "list":
		return []byte(`{"kubernetes/":{"type":"kubernetes"}}`), nil
	case args[0] == "secrets" && args[1] == "list":
		return []byte(`{"oberth/":{"type":"kv","options":{"version":"2"}},"oberth-transit/":{"type":"transit"},"oberth-pki/":{"type":"pki"}}`), nil
	}
	f.t.Fatalf("unscripted bao command %v", args)
	return nil, nil
}

// --- A fake cluster: kubectl, helm and the Kubernetes API ---

type fakeBackupCluster struct {
	helmShowChart []string
	missingChart  bool
	t             *testing.T
	mu            sync.Mutex
	bao           *fakeOpenBao
	client        *fake.Clientset
	values        map[string]any
	chart         string
	schema        int
	database      []byte            // what in-pod `oberth backup database` returns
	rawFiles      map[string][]byte // /data files for the stable-copy fallback
	oldServer     bool              // server predates `oberth backup database`
	workflows     []runWorkflow

	argvs       [][]string
	uploads     map[string][]byte
	swapArgs    []string
	deleted     []string
	helmUpgrade []string
	helmValues  map[string]any
	scales      []int32
}

func serverDeploymentFixture(compat backupCompatibility) *appsv1.Deployment {
	replicas := int32(1)
	labels := map[string]string{"app.kubernetes.io/instance": "oberth", "app.kubernetes.io/name": "oberth"}
	nobody := int64(65534)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{RunAsUser: &nobody, RunAsGroup: &nobody, FSGroup: &nobody},
					Containers: []corev1.Container{{
						Name:  "oberth",
						Image: "registry.example/oberth@sha256:abc",
						Env: []corev1.EnvVar{
							{Name: "OBERTH_SCHEMA_IDENTITY", Value: compat.SchemaIdentity},
							{Name: "OBERTH_AUDIT_DOMAIN", Value: compat.AuditDomain},
							{Name: "OBERTH_WITNESS_KEY_INFO", Value: compat.WitnessKeyInfo},
						},
					}},
					Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "oberth-data"},
					}}},
				},
			},
		},
	}
}

func serverPodFixture(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: DefaultNamespace,
			Labels: map[string]string{"app.kubernetes.io/instance": "oberth", "app.kubernetes.io/name": "oberth"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

func newFakeBackupCluster(t *testing.T, compat backupCompatibility, objects ...runtime.Object) *fakeBackupCluster {
	t.Helper()
	objects = append(objects, serverDeploymentFixture(compat), serverPodFixture("oberth-1"), runningOpenBaoPod())
	c := &fakeBackupCluster{
		t:       t,
		bao:     newFakeOpenBao(t),
		client:  fake.NewClientset(objects...),
		values:  map[string]any{"compatibility": map[string]any{"schemaIdentity": compat.SchemaIdentity, "auditDomain": compat.AuditDomain, "witnessKeyInfo": compat.WitnessKeyInfo}, "argo": map[string]any{"namespace": DefaultArgoNamespace}},
		chart:   "oberth-0.17.28",
		schema:  store.LatestMigrationVersion(),
		uploads: map[string][]byte{},
	}
	pods := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	// Scaling the deployment stops or starts its pod, as the controller would.
	c.client.PrependReactor("patch", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		var patch struct {
			Spec struct {
				Replicas int32 `json:"replicas"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(action.(k8stesting.PatchAction).GetPatch(), &patch); err != nil {
			t.Fatal(err)
		}
		c.mu.Lock()
		c.scales = append(c.scales, patch.Spec.Replicas)
		c.mu.Unlock()
		if patch.Spec.Replicas == 0 {
			_ = c.client.Tracker().Delete(pods, DefaultNamespace, "oberth-1")
		} else {
			_ = c.client.Tracker().Add(serverPodFixture("oberth-2"))
		}
		return false, nil, nil
	})
	// The helper pod starts immediately.
	c.client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodRunning
		return false, nil, nil
	})
	return c
}

func (c *fakeBackupCluster) deps(output *bytes.Buffer) backupDeps {
	return backupDeps{
		Deps: Deps{
			Output:       output,
			KubeClient:   c.client,
			ContextName:  "test-context",
			RunCommand:   c.run,
			RunHelm:      c.helm,
			PollInterval: time.Millisecond,
		},
		runStdout: c.run,
		cipher:    fakeEnvelopeCipher{},
		now:       func() time.Time { return time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC) },
		rootToken: testBackupToken,
	}
}

func (c *fakeBackupCluster) run(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if name != "kubectl" {
		c.t.Fatalf("unexpected host command %s %v", name, args)
	}
	argv := append([]string{name}, args...)
	c.argvs = append(c.argvs, argv)
	for _, arg := range argv {
		if strings.Contains(arg, testBackupToken) || strings.Contains(arg, testSecretValue) {
			c.t.Fatalf("secret material in argv: %v", argv)
		}
	}
	if args[0] == "--context" {
		args = args[2:]
	}
	switch args[0] {
	case "get":
		items := []map[string]any{}
		for _, wf := range c.workflows {
			items = append(items, map[string]any{"metadata": map[string]any{"name": wf.Name, "labels": map[string]string{"oberth.ci/run": wf.RunID}}, "status": map[string]any{"phase": wf.Phase}})
		}
		return json.Marshal(map[string]any{"items": items})
	case "delete":
		for _, arg := range args[1:] {
			if strings.HasPrefix(arg, "oberth-oberth-") {
				c.deleted = append(c.deleted, arg)
			}
		}
		return nil, nil
	case "exec":
	default:
		c.t.Fatalf("unexpected kubectl %v", args)
	}
	separator := slices.Index(args, "--")
	container := args[slices.Index(args, "-c")+1]
	pod := args[separator-1]
	command := args[separator+1:]
	if pod == "openbao-0" {
		return c.bao.handle(command, input)
	}
	joined := strings.Join(command, " ")
	switch {
	case joined == "oberth version --schema":
		return []byte(fmt.Sprintf("%d\n", c.schema)), nil
	case joined == "oberth version":
		return []byte("oberth v0.17.28 commit=abc date=today\n"), nil
	case strings.HasPrefix(joined, "oberth backup database"):
		if c.oldServer {
			return nil, errors.New(`kubectl: exit status 2: oberth: usage error: unknown command "backup"`)
		}
		return c.database, nil
	case len(command) > 2 && command[2] == podSQLiteHashesScript:
		var lines []string
		for _, suffix := range []string{"", "-wal"} {
			if body, ok := c.rawFiles[serverDatabasePath+suffix]; ok {
				sum := sha256.Sum256(body)
				lines = append(lines, hex.EncodeToString(sum[:])+"  "+serverDatabasePath+suffix)
			}
		}
		return []byte(strings.Join(lines, "\n") + "\n"), nil
	case command[0] == "cat":
		return c.rawFiles[command[1]], nil
	case container == restoreHelperContainer && command[2] == stageResetScript:
		return nil, nil
	case container == restoreHelperContainer && command[2] == stageUploadScript:
		c.uploads[command[4]] = append([]byte(nil), input...)
		return nil, nil
	case container == restoreHelperContainer && command[2] == swapScript:
		c.swapArgs = command[4:]
		return []byte("65534:65534 600\n"), nil
	}
	c.t.Fatalf("unscripted in-pod command %v in %s/%s", command, pod, container)
	return nil, nil
}

func (c *fakeBackupCluster) helm(_ context.Context, args []string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case args[0] == "list":
		return json.Marshal([]map[string]any{{"name": "oberth", "namespace": DefaultNamespace, "chart": c.chart, "status": "deployed"}})
	case args[0] == "get" && args[1] == "values":
		return json.Marshal(c.values)
	case args[0] == "repo":
		return nil, nil
	case args[0] == "show" && args[1] == "chart":
		c.helmShowChart = args
		if c.missingChart {
			return nil, errors.New("chart not found")
		}
		return []byte("name: oberth\n"), nil
	case args[0] == "upgrade":
		c.helmUpgrade = args
		file := args[slices.Index(args, "-f")+1]
		body, err := os.ReadFile(file)
		if err != nil {
			c.t.Fatalf("read restore values file: %v", err)
		}
		if err := json.Unmarshal(body, &c.helmValues); err != nil {
			c.t.Fatal(err)
		}
		return nil, nil
	}
	c.t.Fatalf("unscripted helm %v", args)
	return nil, nil
}

// sourceDatabase creates a database under the source protocol with one run,
// returning the in-pod archive bytes, the raw files, and the run's ID.
func sourceDatabase(t *testing.T) ([]byte, map[string][]byte, string) {
	t.Helper()
	ctx := context.Background()
	opts := store.Options{Protocol: store.ProtocolConfig{SchemaIdentity: sourceCompat.SchemaIdentity, AuditDomain: sourceCompat.AuditDomain}}
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.CreateGenesis(ctx, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "forgejo", BaseURL: "https://codeberg.org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main",
		SHA: strings.Repeat("a", 40), Actor: "SHA256:test", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	// An audited action makes the hash chain depend on the audit domain.
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "fixture", Action: "backup-test", ResourceType: "backup", ResourceID: "restore"}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if _, err := store.WriteVerifiedBackup(ctx, path, store.LatestMigrationVersion(), opts, &archive); err != nil {
		t.Fatal(err)
	}
	raw := map[string][]byte{}
	for _, suffix := range []string{"", "-wal"} {
		if body, err := os.ReadFile(path + suffix); err == nil {
			raw[serverDatabasePath+suffix] = body
		}
	}
	return archive.Bytes(), raw, run.ID
}

func seedSourceOpenBao(bao *fakeOpenBao) {
	bao.kv["upstream/codeberg/org/ssh-key"] = map[string]any{"private_key": testSecretValue}
	bao.kv["release/cosign-secret"] = map[string]any{"key": "cosign-canary", "password": "pw-canary"}
	bao.kv["identities/oberth/ssh-host"] = map[string]any{"ed25519": "host-key-canary"}
	bao.policies["oberth-ci"] = "path \"oberth/data/*\" { capabilities = [\"read\"] }"
	bao.policies["oberth-argo-codeberg-org-repo"] = "path \"oberth/data/upstream/codeberg/org/repo/*\" { capabilities = [\"read\"] }"
	bao.roles["oberth-argo-codeberg-org-repo"] = map[string]any{
		"alias_name_source":                "serviceaccount_uid",
		"audience":                         "openbao",
		"bound_service_account_names":      []any{"oberth-argo-codeberg-org-repo"},
		"bound_service_account_namespaces": []any{"oberth-argo"},
		"token_no_default_policy":          true,
		"token_policies":                   []any{"oberth-argo-codeberg-org-repo"},
		"policies":                         []any{"oberth-argo-codeberg-org-repo"},
		"token_ttl":                        float64(1200),
		"token_max_ttl":                    float64(1800),
		"token_bound_cidrs":                []any{},
		"token_type":                       "default",
	}
}

func grantsConfigMap(grants string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: secretAccessConfigMapName, Namespace: DefaultNamespace},
		Data:       map[string]string{secretAccessConfigMapKey: grants},
	}
}

const testGrants = "- repo: codeberg/org/repo\n  step: '*'\n  secret: oberth/data/release/cosign-secret\n"

// backupFixtureArchive runs a full backup against a seeded source cluster.
func backupFixtureArchive(t *testing.T, oldServer bool) (string, string, *fakeBackupCluster) {
	t.Helper()
	archiveBytes, raw, runID := sourceDatabase(t)
	source := newFakeBackupCluster(t, sourceCompat, grantsConfigMap(testGrants))
	source.database, source.rawFiles, source.oldServer = archiveBytes, raw, oldServer
	source.values["argo"] = map[string]any{"namespace": DefaultArgoNamespace, "releaseWIF": map[string]any{
		"namespace": DefaultArgoNamespace, "roles": map[string]any{}, "repositories": map[string]any{},
	}}
	seedSourceOpenBao(source.bao)
	output := filepath.Join(t.TempDir(), "state.cms")
	var out bytes.Buffer
	cfg := BackupConfig{Output: output, Recipient: "recipient.pem", ToolVersion: "v0.17.28"}
	cfg.applyDefaults()
	if err := runBackup(context.Background(), cfg, source.deps(&out)); err != nil {
		t.Fatalf("backup: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), testSecretValue) {
		t.Fatal("backup output prints a secret value")
	}
	return output, runID, source
}

func TestBackupCapturesOpenBaoDatabaseAndValues(t *testing.T) {
	t.Parallel()
	output, _, source := backupFixtureArchive(t, false)
	if len(source.bao.writes) != 0 {
		t.Fatalf("backup wrote to OpenBao: %v", source.bao.writes)
	}
	work := t.TempDir()
	manifest, err := openBackupArchive(context.Background(), output, fakeEnvelopeCipher{}, work)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Compatibility != sourceCompat {
		t.Fatalf("compatibility = %+v, want the deployment's %+v", manifest.Compatibility, sourceCompat)
	}
	if manifest.Database.Snapshot != snapshotInPod || manifest.Database.Schema != store.LatestMigrationVersion() {
		t.Fatalf("database summary = %+v", manifest.Database)
	}
	if manifest.OpenBao.Secrets != 3 || manifest.OpenBao.Policies != 2 || manifest.OpenBao.Roles != 1 {
		t.Fatalf("openbao summary = %+v", manifest.OpenBao)
	}
	var roles rolesExport
	if err := readBackupJSON(work, backupMemberRoles, &roles); err != nil {
		t.Fatal(err)
	}
	// Every field the server returned is kept, including the one a
	// hand-picked restore once dropped.
	if got, want := len(roles.Roles[0].Fields), len(source.bao.roles["oberth-argo-codeberg-org-repo"]); got != want {
		t.Fatalf("role fields = %d, want all %d", got, want)
	}
	if roles.Roles[0].Fields["token_no_default_policy"] != true {
		t.Fatal("token_no_default_policy missing from the role export")
	}
	var policies []policyExport
	if err := readBackupJSON(work, backupMemberPolicies, &policies); err != nil {
		t.Fatal(err)
	}
	for _, policy := range policies {
		if builtinPolicies[policy.Name] {
			t.Fatalf("built-in policy %s exported", policy.Name)
		}
	}
	var reference openBaoReference
	if err := readBackupJSON(work, backupMemberReference, &reference); err != nil {
		t.Fatal(err)
	}
	if _, ok := reference.TransitKeys["oberth-transit/trusted-plan-artifacts"]; !ok || reference.PKICACerts["oberth-pki"] == "" {
		t.Fatalf("reference lacks Transit/PKI metadata: %+v", reference)
	}
	var grants secretAccessExport
	if err := readBackupJSON(work, backupMemberSecretAccess, &grants); err != nil {
		t.Fatal(err)
	}
	if !grants.Present || grants.Data[secretAccessConfigMapKey] != testGrants {
		t.Fatalf("secret-access export = %+v", grants)
	}
}

func TestBackupFallsBackToStableCopyOnOlderServers(t *testing.T) {
	t.Parallel()
	output, _, _ := backupFixtureArchive(t, true)
	work := t.TempDir()
	manifest, err := openBackupArchive(context.Background(), output, fakeEnvelopeCipher{}, work)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Database.Snapshot != snapshotStableCopy {
		t.Fatalf("snapshot = %q, want %q", manifest.Database.Snapshot, snapshotStableCopy)
	}
	protocol := store.ProtocolConfig{SchemaIdentity: sourceCompat.SchemaIdentity, AuditDomain: sourceCompat.AuditDomain}
	if _, err := verifyDatabaseArchive(context.Background(), filepath.Join(work, filepath.FromSlash(backupMemberDatabase)), manifest.Database.Schema, protocol); err != nil {
		t.Fatal(err)
	}
}

func TestBackupDryRunReadsNoSecrets(t *testing.T) {
	t.Parallel()
	cluster := newFakeBackupCluster(t, sourceCompat)
	seedSourceOpenBao(cluster.bao)
	output := filepath.Join(t.TempDir(), "state.cms")
	var out bytes.Buffer
	cfg := BackupConfig{Output: output, Recipient: "recipient.pem", DryRun: true}
	cfg.applyDefaults()
	if err := runBackup(context.Background(), cfg, cluster.deps(&out)); err != nil {
		t.Fatal(err)
	}
	for _, argv := range cluster.argvs {
		if slices.Contains(argv, "openbao-0") {
			t.Fatalf("dry run talked to OpenBao: %v", argv)
		}
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("dry run wrote the archive")
	}
}

// restoreTargetCluster is a fresh install: default compatibility values, the
// installer's own policy text, no grants, and two workflows — one for a run
// the restored database knows, one only the fresh database knew.
func restoreTargetCluster(t *testing.T, knownRunID string) *fakeBackupCluster {
	target := newFakeBackupCluster(t, defaultCompat)
	target.bao.policies["oberth-ci"] = "# installer default"
	target.bao.kv["identities/oberth/ssh-host"] = map[string]any{"ed25519": "fresh-host-key"}
	target.workflows = []runWorkflow{
		{Name: "oberth-oberth-known", RunID: knownRunID, Phase: "Running"},
		{Name: "oberth-oberth-orphan", RunID: strings.Repeat("f", 32), Phase: "Running"},
	}
	return target
}

func TestRestoreAppliesTheArchive(t *testing.T) {
	t.Parallel()
	archive, runID, source := backupFixtureArchive(t, false)
	target := restoreTargetCluster(t, runID)
	var out bytes.Buffer
	cfg := RestoreConfig{Input: archive, Yes: true, Timeout: 10 * time.Second}
	cfg.applyDefaults()
	if err := runRestore(context.Background(), cfg, target.deps(&out)); err != nil {
		t.Fatalf("restore: %v\n%s", err, out.String())
	}
	text := out.String()
	if strings.Contains(text, testSecretValue) {
		t.Fatal("restore output prints a secret value")
	}

	// OpenBao now matches the source field-for-field.
	for path, data := range source.bao.kv {
		if canonicalJSON(target.bao.kv[path]) != canonicalJSON(data) {
			t.Fatalf("secret %s not restored", path)
		}
	}
	for name, rules := range source.bao.policies {
		if strings.TrimSpace(target.bao.policies[name]) != rules {
			t.Fatalf("policy %s = %q, want %q", name, target.bao.policies[name], rules)
		}
	}
	role := target.bao.roles["oberth-argo-codeberg-org-repo"]
	if role["token_no_default_policy"] != true || role["alias_name_source"] != "serviceaccount_uid" {
		t.Fatalf("role restored without every field: %v", role)
	}
	if _, alias := role["policies"]; alias {
		t.Fatal("mirroring deprecated alias was written next to token_policies")
	}

	// The server was stopped, the database swapped through the helper pod,
	// and the server started again.
	if !slices.Equal(target.scales, []int32{0, 1}) {
		t.Fatalf("scales = %v, want [0 1]", target.scales)
	}
	if len(target.uploads["oberth.sqlite"]) == 0 {
		t.Fatal("database was not uploaded to the helper pod")
	}
	sum := sha256.Sum256(target.uploads["oberth.sqlite"])
	if len(target.swapArgs) != 3 || target.swapArgs[0] != "20261017T093000Z" || target.swapArgs[1] != hex.EncodeToString(sum[:]) {
		t.Fatalf("swap args = %v", target.swapArgs)
	}
	if pods, _ := target.client.CoreV1().Pods(DefaultNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=oberth-restore"}); len(pods.Items) != 0 {
		t.Fatal("helper pod left behind")
	}

	// The grants ConfigMap the server reconciles from is back.
	cm, err := target.client.CoreV1().ConfigMaps(DefaultNamespace).Get(context.Background(), secretAccessConfigMapName, metav1.GetOptions{})
	if err != nil || cm.Data[secretAccessConfigMapKey] != testGrants {
		t.Fatalf("secret-access ConfigMap = %v, %v", cm, err)
	}

	// Only the workflow unknown to the restored database was deleted.
	if !slices.Equal(target.deleted, []string{"oberth-oberth-orphan"}) {
		t.Fatalf("deleted workflows = %v", target.deleted)
	}

	// Compatibility values and releaseWIF went onto the release.
	if !slices.Contains(target.helmUpgrade, "--reuse-values") || !slices.Contains(target.helmUpgrade, "oberth-charts/oberth") ||
		!slices.Contains(target.helmUpgrade, "0.17.28") {
		t.Fatalf("helm upgrade args = %v", target.helmUpgrade)
	}
	compat, _ := target.helmValues["compatibility"].(map[string]any)
	if compat["schemaIdentity"] != sourceCompat.SchemaIdentity || compat["auditDomain"] != sourceCompat.AuditDomain || compat["witnessKeyInfo"] != sourceCompat.WitnessKeyInfo {
		t.Fatalf("compatibility values = %v", target.helmValues)
	}
	if argo, _ := target.helmValues["argo"].(map[string]any); argo["releaseWIF"] == nil {
		t.Fatalf("releaseWIF not reapplied: %v", target.helmValues)
	}
	assertSubstringsInOrder(t, text, []string{"Restore plan", "Restoring OpenBao", "Stopping the server", "Swapping the database",
		"oberth install --install-secretstore"})
}

func TestRestoreDryRunChangesNothing(t *testing.T) {
	t.Parallel()
	archive, runID, _ := backupFixtureArchive(t, false)
	target := restoreTargetCluster(t, runID)
	var out bytes.Buffer
	cfg := RestoreConfig{Input: archive, DryRun: true, Timeout: time.Second}
	cfg.applyDefaults()
	if err := runRestore(context.Background(), cfg, target.deps(&out)); err != nil {
		t.Fatalf("restore --dry-run: %v\n%s", err, out.String())
	}
	assertNoRestoreMutation(t, target)
	assertSubstringsInOrder(t, out.String(), []string{"Archive verified", "write 3 secrets (2 new, 1 replacing", "1 replaced",
		"scale deploy/oberth 1 -> 0", "pre-restore-20261017T093000Z", "set argo, compatibility", "oberth-oberth-orphan", "No changes were made"})
}

func TestRestoreWithoutConfirmationChangesNothing(t *testing.T) {
	t.Parallel()
	archive, runID, _ := backupFixtureArchive(t, false)
	target := restoreTargetCluster(t, runID)
	var out bytes.Buffer
	cfg := RestoreConfig{Input: archive, Timeout: time.Second}
	cfg.applyDefaults()
	err := runRestore(context.Background(), cfg, target.deps(&out))
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("unconfirmed restore error = %v, want a --yes refusal", err)
	}
	assertNoRestoreMutation(t, target)
}

func TestRestoreRefusesAnOlderTargetServer(t *testing.T) {
	t.Parallel()
	archive, runID, _ := backupFixtureArchive(t, false)
	target := restoreTargetCluster(t, runID)
	target.schema = store.LatestMigrationVersion() - 1
	var out bytes.Buffer
	cfg := RestoreConfig{Input: archive, Yes: true, Timeout: time.Second}
	cfg.applyDefaults()
	err := runRestore(context.Background(), cfg, target.deps(&out))
	if err == nil || !strings.Contains(err.Error(), "upgrade the target") {
		t.Fatalf("older target error = %v", err)
	}
	assertNoRestoreMutation(t, target)
}

func TestRestoreRefusesADatabaseUnderTheWrongAuditDomain(t *testing.T) {
	t.Parallel()
	archive, runID, _ := backupFixtureArchive(t, false)
	// Rewrite the manifest's audit domain: the chain no longer verifies.
	work := t.TempDir()
	manifest, err := openBackupArchive(context.Background(), archive, fakeEnvelopeCipher{}, work)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Compatibility.AuditDomain = "oberth-audit-v1"
	rewritten := filepath.Join(t.TempDir(), "rewritten.cms")
	if err := writeBackupArchive(context.Background(), work, &manifest, fakeEnvelopeCipher{}, rewritten); err != nil {
		t.Fatal(err)
	}
	target := restoreTargetCluster(t, runID)
	var out bytes.Buffer
	cfg := RestoreConfig{Input: rewritten, Yes: true, Timeout: time.Second}
	cfg.applyDefaults()
	err = runRestore(context.Background(), cfg, target.deps(&out))
	if err == nil || !strings.Contains(err.Error(), "audit domain") {
		t.Fatalf("wrong audit domain error = %v", err)
	}
	assertNoRestoreMutation(t, target)
}

func TestRestoreFailsWhenARoleDoesNotReadBack(t *testing.T) {
	t.Parallel()
	archive, runID, _ := backupFixtureArchive(t, false)
	target := restoreTargetCluster(t, runID)
	target.bao.dropOnRoleWrite = "token_no_default_policy"
	var out bytes.Buffer
	cfg := RestoreConfig{Input: archive, Yes: true, Timeout: time.Second}
	cfg.applyDefaults()
	err := runRestore(context.Background(), cfg, target.deps(&out))
	if err == nil || !strings.Contains(err.Error(), "oberth-argo-codeberg-org-repo (token_no_default_policy)") {
		t.Fatalf("lossy role write error = %v, want the role and field named", err)
	}
	if len(target.scales) != 0 {
		t.Fatal("server was stopped although OpenBao verification failed")
	}
}

func assertNoRestoreMutation(t *testing.T, target *fakeBackupCluster) {
	t.Helper()
	if len(target.bao.writes) != 0 || len(target.scales) != 0 || len(target.uploads) != 0 ||
		len(target.deleted) != 0 || target.helmUpgrade != nil {
		t.Fatalf("cluster changed: bao writes %v, scales %v, uploads %d, deleted %v, helm %v",
			target.bao.writes, target.scales, len(target.uploads), target.deleted, target.helmUpgrade)
	}
	if _, err := target.client.CoreV1().ConfigMaps(DefaultNamespace).Get(context.Background(), secretAccessConfigMapName, metav1.GetOptions{}); err == nil {
		t.Fatal("secret-access ConfigMap was written")
	}
}

func TestRoleWritePayloadKeepsEveryFieldButMirroredAliases(t *testing.T) {
	t.Parallel()
	fields := map[string]any{
		"token_no_default_policy": false,
		"token_policies":          []any{"a"},
		"policies":                []any{"a"},
		"token_ttl":               json.Number("600"),
		"ttl":                     float64(300), // distinct: kept as written
		"audience":                "",
	}
	payload := roleWritePayload(fields)
	if _, ok := payload["policies"]; ok {
		t.Fatal("mirroring alias kept")
	}
	for _, key := range []string{"token_no_default_policy", "token_policies", "token_ttl", "ttl", "audience"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("field %s dropped", key)
		}
	}
	if diff := diffRole(fields, map[string]any{"token_no_default_policy": false, "token_policies": []any{"a"}, "token_ttl": float64(600), "ttl": float64(300), "audience": "", "token_type": "default"}); len(diff) != 0 {
		t.Fatalf("equivalent role differs in %v", diff)
	}
	if diff := diffRole(fields, map[string]any{"token_policies": []any{"a"}, "token_ttl": float64(600), "ttl": float64(300), "audience": ""}); !slices.Equal(diff, []string{"token_no_default_policy"}) {
		t.Fatalf("diff = %v, want token_no_default_policy", diff)
	}
}

// A chart that cannot be resolved stops the restore before the server is
// scaled down, not after the database was swapped (review finding).
func TestRestoreChecksTheChartBeforeStoppingTheServer(t *testing.T) {
	t.Parallel()
	archive, runID, _ := backupFixtureArchive(t, false)
	target := restoreTargetCluster(t, runID)
	target.missingChart = true
	var out bytes.Buffer
	cfg := RestoreConfig{Input: archive, Yes: true, Timeout: 10 * time.Second}
	cfg.applyDefaults()
	err := runRestore(context.Background(), cfg, target.deps(&out))
	if err == nil || !strings.Contains(err.Error(), "is not available") {
		t.Fatalf("restore with a missing chart = %v", err)
	}
	if len(target.scales) != 0 {
		t.Fatalf("the server was scaled (%v) before the chart was proven available", target.scales)
	}
}

// The swap script puts the previous database back when it fails after
// moving files aside, so the server never starts on an empty volume.
func TestSwapScriptRollsBackOnFailure(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(data, ".oberth-restore"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "oberth.sqlite"), []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	restored := []byte("restored")
	if err := os.WriteFile(filepath.Join(data, ".oberth-restore", "oberth.sqlite"), restored, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(restored)
	// Run the script against the temp dir, with stat failing as a late step.
	script := strings.Replace(swapScript, "cd /data", "cd "+data, 1)
	script = strings.Replace(script, "stat -c '%u:%g %a' oberth.sqlite", "swapping=1; false", 1)
	cmd := exec.Command("sh", "-c", script, "sh", "20261017T000000Z", hex.EncodeToString(sum[:]), "-")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected the forced failure, got success: %s", out)
	}
	got, err := os.ReadFile(filepath.Join(data, "oberth.sqlite"))
	if err != nil || string(got) != "previous" {
		t.Fatalf("previous database not put back: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(data, "pre-restore-20261017T000000Z")); !os.IsNotExist(err) {
		t.Fatalf("aside directory should be removed after rollback: %v", err)
	}
}
