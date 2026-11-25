package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"time"

	wfclient "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
	"strings"
)

// serverRuntime contains composition settings; workflow defaults and release
// routes retain their owning contracts instead of a second policy decoder.
type serverRuntime struct {
	ReceiveMaxInputBytes    int64                                  `json:"receive_max_input_bytes"`
	GitBinary               string                                 `json:"git_binary"`
	SourceTimeoutSeconds    int                                    `json:"source_timeout_seconds"`
	AdmissionTimeoutSeconds int                                    `json:"admission_timeout_seconds"`
	SourceClientBurst       int                                    `json:"source_client_burst"`
	DefaultBranch           string                                 `json:"default_branch"`
	ImagePrefixes           []string                               `json:"image_prefixes"`
	HelperImage             string                                 `json:"helper_image"`
	Defaults                runtimeconfig.WorkflowDefaultsSnapshot `json:"workflow_defaults"`
	SourcePolicy            json.RawMessage                        `json:"source_policy"`
	Routes                  workflowpolicy.ReleaseRoutes           `json:"release_routes"`
	IssueStaleSeconds       int                                    `json:"issue_stale_after_seconds"`
	IssueActiveStaleSeconds int                                    `json:"issue_active_stale_after_seconds"`
}

// An installation stale window lasts from a minute to a year.
const minIssueWindowSeconds, maxIssueWindowSeconds = 60, 365 * 24 * 3600

func decodeRuntime(raw []byte) (serverRuntime, error) {
	var r serverRuntime
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return r, serverProblem("config", "runtime: "+err.Error(), err)
	}
	if bytes.Equal(r.Defaults.WorkflowDefaults, []byte("null")) {
		r.Defaults.WorkflowDefaults = nil
	}
	return r, validateRuntime(r)
}
func validateRuntime(r serverRuntime) error {
	if !filepath.IsAbs(r.GitBinary) {
		return serverProblem("config", "runtime.git_binary: expected an absolute executable path", nil)
	}
	var problems []string
	for field, valid := range map[string]bool{
		"runtime.source_timeout_seconds":           r.SourceTimeoutSeconds >= 1 && r.SourceTimeoutSeconds <= 3600,
		"runtime.admission_timeout_seconds":        r.AdmissionTimeoutSeconds >= 1 && r.AdmissionTimeoutSeconds <= 3600,
		"runtime.receive_max_input_bytes":          r.ReceiveMaxInputBytes >= 0,
		"runtime.source_client_burst":              r.SourceClientBurst >= 3,
		"runtime.default_branch":                   strings.HasPrefix(r.DefaultBranch, "refs/heads/") && sourcecontract.ValidatePublicRef(r.DefaultBranch) == nil,
		"runtime.issue_stale_after_seconds":        validIssueWindow(r.IssueStaleSeconds),
		"runtime.issue_active_stale_after_seconds": validIssueWindow(r.IssueActiveStaleSeconds),
	} {
		if !valid {
			problems = append(problems, field+": invalid or missing value")
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return serverProblem("config", strings.Join(problems, "\n"), nil)
	}
	return r.Routes.Validate()
}

type clusterClients struct {
	Kube           kubernetes.Interface
	Argo           wfclient.Interface
	REST           *rest.Config
	collectionKube kubernetes.Interface
	collectionArgo wfclient.Interface
	deliveryReview kubernetes.Interface
	deliveryKube   kubernetes.Interface
	deliveryArgo   wfclient.Interface
	backupReview   kubernetes.Interface
}

// The server's cluster clients send at most serverClientQPS requests a second
// after a burst of serverClientBurst, instead of the library's 5 and 10. One
// admission reads each distinct account at most twice: a 40-step push makes two
// reads, and one whose 40 steps each hold their own secret makes 82, which the
// burst still serves without waiting. The API server's priority and fairness
// remains the limiter that protects the cluster.
const serverClientQPS, serverClientBurst = 50, 100

func inClusterClients(context.Context) (clusterClients, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return clusterClients{}, err
	}
	return newClusterClients(cfg)
}
func newClusterClients(cfg *rest.Config) (clusterClients, error) {
	cfg.QPS, cfg.Burst = serverClientQPS, serverClientBurst
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return clusterClients{}, err
	}
	argo, err := wfclient.NewForConfig(cfg)
	if err != nil {
		return clusterClients{}, err
	}
	collection := rest.CopyConfig(cfg)
	collection.QPS, collection.Burst = argojob.CollectionClientQPS, argojob.CollectionClientBurst
	collection.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(collection.QPS, collection.Burst)
	collectionKube, err := kubernetes.NewForConfig(collection)
	if err != nil {
		return clusterClients{}, err
	}
	collectionArgo, err := wfclient.NewForConfig(collection)
	if err != nil {
		return clusterClients{}, err
	}
	backupReview, err := kubernetes.NewForConfig(budget(cfg, backupReviewQPS, backupReviewBurst))
	if err != nil {
		return clusterClients{}, err
	}
	review, deliveryKube, deliveryArgo, err := deliveryClients(cfg)
	return clusterClients{Kube: kube, Argo: argo, REST: cfg, collectionKube: collectionKube, collectionArgo: collectionArgo, deliveryReview: review, deliveryKube: deliveryKube, deliveryArgo: deliveryArgo, backupReview: backupReview}, err
}

type serverPlatform struct {
	cluster    func(context.Context) (clusterClients, error)
	executable string
}

type serverOwners struct {
	pinsMu    sync.Mutex
	checks    *app.Checks
	c         *composition
	platform  serverPlatform
	settings  serverRuntime
	cluster   clusterClients
	cache     *gitcache.Cache
	notices   *refNotices
	wiring    *app.Submitter
	authority *app.AuthorityReader
	native    *argojob.Executor
	service   *service.Service
	podAt     func(ip string) string
}

func (p serverPlatform) constructors() constructors {
	return constructors{
		CacheNew: func(ctx context.Context, c *composition) (Part, error) {
			settings := c.settings
			clients, err := p.cluster(ctx)
			if err != nil {
				return Part{}, serverProblem("config", "cluster: cannot construct native clients", err)
			}
			c.owners = &serverOwners{c: c, platform: p, settings: settings, cluster: clients}
			return c.owners.cachePart(ctx)
		},
		NativeNew:  func(ctx context.Context, c *composition) (Part, error) { return c.owners.nativePart(ctx) },
		WiringNew:  func(ctx context.Context, c *composition) (Part, error) { return c.owners.wiringPart(ctx) },
		ServiceNew: func(ctx context.Context, c *composition) (Part, error) { return c.owners.servicePart(ctx) },
		SourceNew:  func(ctx context.Context, c *composition) (Part, error) { return c.owners.sourcePart(ctx) },
	}
}
func (o *serverOwners) notice(err error) {
	if err != nil {
		_, _ = fmt.Fprintln(o.c.diagnostic, err)
	}
}
func (o *serverOwners) sourceTimeout() time.Duration {
	return time.Duration(o.settings.SourceTimeoutSeconds) * time.Second
}

func validIssueWindow(seconds int) bool {
	return seconds >= minIssueWindowSeconds && seconds <= maxIssueWindowSeconds
}

// issueWindows are the stale windows of issue reads that name none.
func (r serverRuntime) issueWindows() model.IssueWindows {
	return model.IssueWindows{General: time.Duration(r.IssueStaleSeconds) * time.Second, Active: time.Duration(r.IssueActiveStaleSeconds) * time.Second}
}
