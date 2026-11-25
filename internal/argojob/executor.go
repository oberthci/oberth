package argojob

import (
	"context"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"golang.org/x/time/rate"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
)

// PatchRequest carries a fenced native JSON patch.
type PatchRequest struct {
	Name string
	Body []byte
}

// Workflows creates, reads and patches one namespace's Workflows.
type Workflows interface {
	Create(context.Context, *wf.Workflow, meta.CreateOptions) (*wf.Workflow, error)
	Get(context.Context, string, meta.GetOptions) (*wf.Workflow, error)
	Patch(context.Context, PatchRequest) (*wf.Workflow, error)
}

// Accounts reads and creates the namespace-scoped accounts admission names.
type Accounts interface {
	Get(context.Context, string, meta.GetOptions) (*core.ServiceAccount, error)
	Create(context.Context, *core.ServiceAccount, meta.CreateOptions) (*core.ServiceAccount, error)
}

// Pods supplies live ownership reads, exec and log streams.
type Pods interface {
	Get(context.Context, string, meta.GetOptions) (*core.Pod, error)
	Exec(context.Context, ExecRequest) error
	Logs(context.Context, string, core.PodLogOptions) (io.ReadCloser, error)
}

type ExecRequest struct {
	Pod, Container string
	Command        []string
	In             io.Reader
	Out, Err       io.Writer
}

// Cache supplies copied observations; membership grants no authority.
type Cache interface {
	Workflow(string) (*wf.Workflow, bool)
	Pods(string) []*core.Pod
	OnChange(func(string))
}

// Metrics holds native observation counters without per-run metric labels.
type Metrics struct{ IncompleteEvidence atomic.Uint64 }

// NativeClients lets collection use a transport budget separate from live work.
type NativeClients struct {
	Workflows Workflows
	Pods      Pods
}

type Config struct {
	InstanceID         string
	SecretStore        runtimeconfig.SecretDelivery
	Children           Children
	ChildRuns          ChildRuns
	Keeper             Keeper
	ObservationTimeout time.Duration
	Metrics            *Metrics
	Progress           ProgressStore
	Reporter           Reporter
	Logs               LogStore
	Workflows          Workflows
	Accounts           Accounts
	Pods               Pods
	Collection         NativeClients
	Runs               Runs
	Logger             *slog.Logger
	RateLimiter        workqueue.TypedRateLimiter[string]
	Cache              Cache
	Now                func() time.Time
}
type Executor struct {
	collector        *Executor
	live             *Executor
	storeConfigured  bool
	config           Config
	mu               sync.Mutex
	tracked          map[string]*tracking
	recoveryErrors   map[string]string
	childNotices     map[string]childNotice
	queue            workqueue.TypedRateLimitingInterface[string]
	captureReady     chan struct{}
	collectionReads  *rate.Limiter
	collectionWindow time.Time
	collectionCount  int
}

func New(c Config) *Executor {
	e := newExecutor(c)
	collection := e.config
	collection.RateLimiter = nil
	if c.Collection.Workflows != nil {
		collection.Workflows = c.Collection.Workflows
	}
	if c.Collection.Pods != nil {
		collection.Pods = c.Collection.Pods
	}
	e.collector = newExecutor(collection)
	e.collector.live = e
	if c.Cache != nil {
		c.Cache.OnChange(e.changed)
	}
	return e
}

func newExecutor(c Config) *Executor {
	if c.ObservationTimeout <= 0 {
		c.ObservationTimeout = 30 * time.Second
	}
	if c.Metrics == nil {
		c.Metrics = &Metrics{}
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if c.RateLimiter == nil {
		c.RateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Second, 8*time.Second)
	}
	e := &Executor{storeConfigured: c.SecretStore.Valid(), captureReady: make(chan struct{}, 1), config: c, tracked: make(map[string]*tracking), recoveryErrors: map[string]string{}, childNotices: map[string]childNotice{}, queue: workqueue.NewTypedRateLimitingQueue(c.RateLimiter)}
	e.collectionReads = newCollectionReads()
	return e
}
