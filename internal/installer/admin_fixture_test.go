package installer

// These adapters retain the business-state fixtures used by onboarding,
// backup and upgrade tests while production callers use AdminAPI. No process
// is executed: the runner is a test fake, never DefaultRunCommand.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/client-go/kubernetes"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/recordjson"
)

type fixtureAdmin struct {
	authenticated      func() bool
	existing           func() bool
	run                CommandRunner
	namespace, context string
}

func (f fixtureAdmin) command(ctx context.Context, input []byte, words ...string) ([]byte, error) {
	args := []string{"exec", "-i", "-c", "oberth"}
	if f.context != "" {
		args = append(args, "--context", f.context)
	}
	ns := f.namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	args = append(args, "-n", ns, "deploy/oberth", "--", "/usr/local/bin/oberth")
	args = append(args, words...)
	return f.run(ctx, input, "kubectl", args...)
}
func fixtureDecode(value, into any) error {
	if into == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return recordjson.Unmarshal(raw, into)
}
func fixtureRequest(value, into any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}
func (f fixtureAdmin) Admin(ctx context.Context, action string, args, into any) error {
	switch action {
	case "upstream-list":
		var request struct {
			Check bool `json:"check"`
		}
		if args != nil {
			if err := fixtureRequest(args, &request); err != nil {
				return err
			}
		}
		words := []string{"upstream", "list"}
		if request.Check {
			words = append(words, "--check")
		}
		raw, err := f.command(ctx, nil, words...)
		if err != nil {
			return err
		}
		rows := []map[string]any{}
		authAt := -1
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			if fields[0] == "NAME" {
				authAt = strings.Index(line, "AUTH")
				continue
			}
			auth := "ok"
			if authAt >= 0 && len(line) > authAt {
				auth = strings.TrimSpace(line[authAt:])
			}
			rows = append(rows, map[string]any{"upstream": map[string]any{"name": fields[0], "kind": fields[1], "base_url": fields[2]}, "auth": auth})
		}
		return fixtureDecode(map[string]any{"upstreams": rows}, into)
	case "upstream-add":
		var request api.UpstreamAddRequest
		if err := fixtureRequest(args, &request); err != nil {
			return err
		}
		words := []string{"upstream", "add", "--yes"}
		if request.NoWait {
			words = append(words, "--no-wait")
		}
		if request.DedicatedKey {
			words = append(words, "--dedicated-key")
		}
		if request.ExpectedHostFingerprint != "" {
			words = append(words, "--expected-host-fingerprint", request.ExpectedHostFingerprint)
		}
		if request.CheckRepository != "" {
			words = append(words, "--check-repo", request.CheckRepository)
		}
		words = append(words, request.Name, request.BaseURL)
		raw, err := f.command(ctx, nil, words...)
		if err != nil {
			return err
		}
		pending := strings.Contains(string(raw), "pending")
		return fixtureDecode(map[string]any{"identity": api.UpstreamIdentity{PublicKey: fixtureDeployPublicKey(string(raw)), Generated: !strings.Contains(string(raw), persistedDeployKeyMarker), Pending: pending}}, into)
	case "upstream-provide-key":
		var request api.UpstreamKeyRequest
		if err := fixtureRequest(args, &request); err != nil {
			return err
		}
		_, err := f.command(ctx, []byte(request.PrivateKey), "upstream", "provide-key", request.Name)
		return err
	case "public-identity":
		var request struct {
			Name string `json:"name"`
		}
		if err := fixtureRequest(args, &request); err != nil {
			return err
		}
		path := map[string]string{"server-ca": "/run/oberth-identities/server/ca.crt", "server-certificate": "/run/oberth-identities/server/tls.crt", "upstream-public-key": "/run/oberth-identities/upstream/id_ed25519.pub"}[request.Name]
		raw, err := f.command(ctx, nil, "admin", "read-file", path)
		if err != nil {
			return err
		}
		return fixtureDecode(map[string]string{"content": string(raw)}, into)
	case "readiness":
		raw, err := f.command(ctx, nil, "admin", "readiness")
		if err != nil {
			return err
		}
		return recordjson.Unmarshal(raw, into)
	case "version":
		if f.authenticated != nil && !f.authenticated() {
			return &client.AdminError{StatusCode: http.StatusUnauthorized, Cause: errors.New("fixture has no admin enrollment yet")}
		}
		raw, err := f.command(ctx, nil, "version", "--schema")
		if err != nil {
			return err
		}
		schema, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		raw, err = f.command(ctx, nil, "version")
		if err != nil {
			return err
		}
		version := ""
		for _, word := range strings.Fields(string(raw)) {
			if strings.HasPrefix(word, "v") {
				version = word
				break
			}
		}
		return fixtureDecode(map[string]any{"version": version, "schema": schema}, into)
	case "access-allow", "access-revoke":
		var key model.SecretAccessKey
		if err := fixtureRequest(args, &key); err != nil {
			return err
		}
		verb := "allow"
		if action == "access-revoke" {
			verb = "revoke"
		}
		_, err := f.command(ctx, nil, "access", verb, key.Repo, key.Workflow, key.Step, key.Secret)
		return err
	case "access-list":
		raw, err := f.command(ctx, nil, "access", "list", "--json")
		if err != nil {
			return err
		}
		if len(raw) == 0 {
			raw = []byte("[]")
		}
		var grants []accessListJSONEntry
		if err := json.Unmarshal(raw, &grants); err != nil {
			return err
		}
		return fixtureDecode(map[string]any{"grants": grants}, into)
	case "audit-record":
		var request struct {
			Action  string          `json:"action"`
			Details json.RawMessage `json:"details"`
		}
		if err := fixtureRequest(args, &request); err != nil {
			return err
		}
		_, err := f.command(ctx, nil, "audit", "record", request.Action, "--details", string(request.Details))
		return err
	case "idle":
		_, err := f.command(ctx, nil, "wait-idle")
		if err != nil {
			return err
		}
		return fixtureDecode(map[string]any{"busy": false, "queue": map[string]any{"version": 1}}, into)
	default:
		return fmt.Errorf("unhandled fixture admin action %s", action)
	}
}
func (f fixtureAdmin) DownloadAdminBackup(ctx context.Context, limit int64, output io.Writer) error {
	raw, err := f.command(ctx, nil, "backup", "database", "--database", serverDatabasePath, "--max-database-bytes", strconv.FormatInt(limit, 10))
	if err != nil {
		return err
	}
	_, err = output.Write(raw)
	return err
}

var fixtureAdminRoot string

func fixtureClientRoot() func() (string, error) {
	root, err := os.MkdirTemp(fixtureAdminRoot, "admin-fixture-")
	if err != nil {
		panic(err)
	}
	return func() (string, error) { return root, nil }
}
func testAdminDeps(deps Deps) Deps {
	if deps.ClientConfigRoot == nil && usesPackageFixtureHome() {
		deps.ClientConfigRoot = fixtureClientRoot()
	}
	if deps.Admin == nil && deps.RunCommand != nil {
		deps.Admin = fixtureAdmin{run: deps.RunCommand, namespace: DefaultNamespace, context: deps.ContextName}
	}
	return deps
}
func testAdminInstallDeps(deps InstallDeps) InstallDeps {
	if deps.ClientConfigRoot == nil && usesPackageFixtureHome() {
		deps.ClientConfigRoot = fixtureClientRoot()
	}
	if deps.Admin == nil && deps.RunCommand != nil {
		deps.Admin = fixtureAdmin{run: deps.RunCommand, namespace: DefaultNamespace}
	}
	return deps
}
func testProducePerRepoIdentities(ctx context.Context, kube kubernetes.Interface, run CommandRunner, selected, namespace string) ([]PerRepoIdentity, []string, error) {
	if run == nil {
		return nil, nil, errors.New("live admin API is required")
	}
	return ProducePerRepoIdentities(ctx, Config{Namespace: namespace}, Deps{Admin: fixtureAdmin{run: run, namespace: namespace, context: selected}, KubeClient: kube, ContextName: selected})
}
func testProducePerStepIdentities(ctx context.Context, run CommandRunner, selected, namespace string) ([]PerStepIdentity, error) {
	return ProducePerStepIdentities(ctx, Config{Namespace: namespace}, Deps{Admin: fixtureAdmin{run: run, namespace: namespace, context: selected}, ContextName: selected})
}

type streamingFixtureAdmin struct {
	AdminAPI
	download func(context.Context, int64, io.Writer) error
}

func (f streamingFixtureAdmin) DownloadAdminBackup(ctx context.Context, limit int64, out io.Writer) error {
	return f.download(ctx, limit, out)
}

func fixtureDeployPublicKey(output string) string {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "ssh-") {
			return trimmed
		}
	}
	return ""
}

func fixtureUplinkToken(output string) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "Uplink token for ") && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1])
		}
	}
	// Fallback: if only one non-empty line that looks like a token
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "oberth_") {
			return trimmed
		}
	}
	return ""
}

const persistedDeployKeyMarker = "Persisted upstream deploy public key"

func refreshFixtureAdmin(deps any) {
	switch d := deps.(type) {
	case *Deps:
		if d.Admin == nil && d.RunCommand != nil {
			d.Admin = fixtureAdmin{run: d.RunCommand, context: d.ContextName}
		}
		if f, ok := d.Admin.(fixtureAdmin); ok {
			f.run = d.RunCommand
			d.Admin = f
		}
	case *InstallDeps:
		if d.Admin == nil && d.RunCommand != nil {
			d.Admin = fixtureAdmin{run: d.RunCommand}
		}
		if f, ok := d.Admin.(fixtureAdmin); ok {
			f.run = d.RunCommand
			d.Admin = f
		}
	}
}

func usesPackageFixtureHome() bool {
	home, _ := os.UserHomeDir()
	root, _ := client.ConfigDir()
	return home == filepath.Dir(fixtureAdminRoot) && root == filepath.Join(home, ".config", "oberth")
}

func withUpgradeAdminFixture(deps *Deps, version string) {
	if deps.Admin != nil {
		return
	}
	if deps.RunCommand != nil {
		*deps = testAdminDeps(*deps)
		return
	}
	deps.Admin = fixtureAdmin{namespace: DefaultNamespace, context: deps.ContextName, run: func(context.Context, []byte, string, ...string) ([]byte, error) {
		return []byte("oberth v" + version + " commit=fixture"), nil
	}}
}

const secretAccessConfigMapName = "oberth-secret-access"
const secretAccessConfigMapKey = "grants"

type grantEntry struct {
	Workflow string `json:"workflow" yaml:"workflow"`
	Repo     string `json:"repo"   yaml:"repo"`
	Step     string `json:"step"   yaml:"step"`
	Secret   string `json:"secret" yaml:"secret"`
}
