package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/oberthci/oberth/internal/kubeconfig"
	"github.com/oberthci/oberth/internal/secretstore"
	"helm.sh/helm/v3/pkg/chart"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Revision carries user values, never the coalesced defaults of an older chart.
type Revision struct {
	Number          int
	Version, Status string
	Kind            string
	RollbackID      string
	ManifestDigest  string
	Values          map[string]any
}

// Installer reports whether validated source-kind history identifies this revision.
func (r Revision) Installer() bool { return r.Kind == "local" || r.Kind == "verified" }

// History keeps native revision facts and the latest applied installer choices.
type History struct {
	Deployed, Latest, Choices Revision
	Verified                  bool
	Foreign                   []int
}

// ChartPort uses the chart library for rendering, preview and release storage.
// Composition binds all three operations to Engine.Cluster.
type ChartPort interface {
	Read(context.Context, string) (History, error)
	Preview(context.Context, ChartRequest) error
	Apply(context.Context, ChartRequest) error
}

// ChartRequest is shared by preview and apply; it never carries private keys.
type ChartRequest struct {
	RollbackID     string
	Local          bool
	Recovery       *Recovery
	Chart          *chart.Chart
	Namespace      string
	Revision       int
	Predecessor    int
	ManifestDigest string
	Values         map[string]any
}

// Engine binds installation to its explicit target and supplied resources.
type Engine struct {
	Definitions   DefinitionReader
	OpenStore     func(context.Context, secretstore.Config) (*secretstore.Client, func(), error)
	Cluster       kubeconfig.Target
	Charts        ChartPort
	Credentials   *ConfigDir
	Random        io.Reader
	Now           func() time.Time
	CheckEndpoint func(context.Context, Profile) error
	Pause         func(context.Context) error
	// GitHelper points the profile's Git helper at the saved profile's key and
	// returns the files the helper names; false means it could not, so every
	// earlier credential file is kept.
	GitHelper    func(context.Context, Profile) ([]string, bool)
	Timeout      time.Duration
	ChartTimeout time.Duration
}

// Plan is immutable outside the engine; approval binds the exact review digest.
type Plan struct {
	choices                    Choices
	definitions                definitionReview
	podSecurity                string
	portScope                  string
	address, directory, plugin string
	release                    Release
	request                    ChartRequest
	uid                        string
	history                    History
	review                     []string
	digest                     string
	identities                 Identities
	admin                      admin
	previous                   Profile
	minimumSchema              int
}

func (p *Plan) Review() []string { return append([]string(nil), p.review...) }
func (p *Plan) Digest() string   { return p.digest }

// Close clears private prepared buffers after apply or cancellation.
func (p *Plan) Close() { p.digest = ""; p.identities.clear(); clear(p.admin.key) }

// Prepare performs reads and chart dry-run; new identities stay only in memory.
func (e *Engine) Prepare(ctx context.Context, r Release, c Choices) (*Plan, error) {
	return e.prepare(ctx, r, c, 0)
}

// minimumSchema comes from the live database or the authenticated rollback backup.
func (e *Engine) prepare(ctx context.Context, r Release, c Choices, minimumSchema int) (*Plan, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if r.chart == nil || !e.configured() {
		return nil, problem("wiring", nil)
	}
	c.statusSelected = scalarStatusChoices(c)
	c, uid, err := e.target(ctx, c)
	if err != nil {
		return nil, err
	}
	h, err := e.ownedHistory(ctx, c.Namespace)
	if err != nil {
		return nil, chartError("cluster", err)
	}
	h.Deployed.Values = copyValues(h.Deployed.Values)
	h.Choices.Values = copyValues(h.Choices.Values)
	h.Latest.Values = copyValues(h.Latest.Values)
	c, err = e.installChoices(c, h)
	if err != nil {
		return nil, err
	}
	v, err := values(r, c, h.Choices.Values)
	if err != nil {
		return nil, err
	}
	if err = e.readIdentities(ctx, c.Namespace, h.Deployed.Number > 0); err != nil {
		return nil, err
	}
	recovery, err := recoveryFor(h)
	if err != nil {
		return nil, err
	}
	p := &Plan{
		choices:       c,
		minimumSchema: minimumSchema,
		release:       r,
		uid:           uid,
		history:       h,
		request:       ChartRequest{Local: !r.Verified(), Chart: r.chart, Namespace: c.Namespace, Values: v, Recovery: recovery},
	}
	p.request.bindHistory(h)
	p.address, p.directory = e.Cluster.Config.Host, e.Credentials.path
	if login := e.Cluster.Config.ExecProvider; login != nil {
		p.plugin = login.Command
	}
	if err = e.prepareIdentities(ctx, p); err != nil {
		p.Close()
		return nil, err
	}
	p.review = review(p, r.Version())
	if err = e.reviewPendingStore(p); err != nil {
		p.Close()
		return nil, err
	}
	p.rehash()
	return p, nil
}
func (e *Engine) target(ctx context.Context, c Choices) (Choices, string, error) {
	_, err := url.Parse(e.Cluster.Config.Host)
	if err != nil {
		return Choices{}, "", problem("choices", err)
	}
	if c.Context != e.Cluster.Context {
		return Choices{}, "", described("target", nil, "The requested context differs from the selected cluster context.")
	}
	c.Namespace, err = e.installationNamespace(c.Namespace)
	if err != nil {
		return Choices{}, "", err
	}
	uid, err := e.Cluster.UID(ctx)
	if err != nil {
		return Choices{}, "", problem("cluster", err)
	}
	if err = e.checkInstallationTarget(ctx, c, uid); err != nil {
		return Choices{}, "", err
	}
	version, err := e.Cluster.Version(ctx)
	if err != nil {
		return Choices{}, "", wrap("read cluster version", err)
	}
	if !supportedVersion(version) {
		return Choices{}, "", described("version", nil, "Found: "+version+"; minimum: v1.30.0.")
	}
	return c, uid, nil
}
func (e *Engine) prepareIdentities(ctx context.Context, p *Plan) error {
	if err := e.discoverSharedCluster(ctx, p); err != nil {
		return err
	}
	var err error
	p.identities, err = e.identities(ctx, p)
	if err != nil {
		return err
	}
	p.admin, err = e.savedAdministrator(p)
	if err != nil {
		p.Close()
		return err
	}
	if err = p.checkRelease(); err != nil {
		p.Close()
		return err
	}
	p.request.Values = merge(p.request.Values, map[string]any{
		"server":               map[string]any{"runtime": map[string]any{"source": map[string]any{"ca_pem": string(p.identities.CA.Data["tls.crt"])}}},
		"adminPublicKeySHA256": publicDigest(p.admin.public),
	})
	if err = e.Charts.Preview(ctx, p.request); err != nil {
		p.Close()
		return chartError("chart", err)
	}
	return nil
}
func (e *Engine) readIdentities(ctx context.Context, ns string, installed bool) error {
	for _, name := range []string{"oberth-ca", "oberth-ssh-host", "oberth-forge-key", "oberth-store-tls"} {
		s, err := e.Cluster.Client.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) && !installed {
			continue
		}
		if apierrors.IsNotFound(err) {
			return described("identity-missing", err, "Secret: "+ns+"/"+name)
		}
		if err != nil || !validSecret(s) {
			return described("identity", err, "Secret: "+ns+"/"+name)
		}
	}
	return nil
}
func supportedVersion(raw string) bool {
	var major, minor int
	_, err := fmt.Sscanf(raw, "v%d.%d", &major, &minor)
	return err == nil && (major > 1 || (major == 1 && minor >= 30))
}

func (e *Engine) configured() bool {
	return e.Cluster.Config != nil && e.Cluster.Client != nil && e.Definitions != nil && e.Charts != nil && e.Credentials != nil && e.Random != nil && e.Now != nil && e.CheckEndpoint != nil
}

// MarshalJSON exposes the review and its public target, excluding prepared keys.
func (p *Plan) MarshalJSON() ([]byte, error) {
	values, _ := inputs(p.release.chart, p.request.Values, p.choices.Namespace)
	return json.Marshal(struct {
		Version    int               `json:"version"`
		Context    string            `json:"context"`
		ClusterUID string            `json:"cluster_uid"`
		Review     []string          `json:"review"`
		Images     map[string]string `json:"images"`
		Approval   string            `json:"approval_digest"`
	}{1, p.choices.Context, p.uid, p.Review(), reviewImages(values), p.digest})
}

func (p *Plan) rehash() {
	raw, _ := json.Marshal(struct {
		Review   []string
		Values   map[string]any
		History  History
		Recovery *Recovery
	}{p.review, p.request.Values, p.history, p.request.Recovery})
	sum := sha256.Sum256(raw)
	p.digest = hex.EncodeToString(sum[:])
}
