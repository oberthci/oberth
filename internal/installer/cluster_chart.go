package installer

import (
	"context"
	"net/http"
	"time"

	"github.com/oberthci/oberth/internal/kubeconfig"
	"helm.sh/helm/v3/pkg/action"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// NewChartLibrary binds every chart call and release Secret to the selected target.
func NewChartLibrary(target kubeconfig.Target, timeout time.Duration) ChartLibrary {
	return ChartLibrary{target: &target, Timeout: timeout}
}
func (l ChartLibrary) configuration(ctx context.Context, namespace string) (*action.Configuration, error) {
	if l.config != nil {
		return l.config, nil
	}
	if l.target == nil || l.target.Config == nil {
		return nil, problem("choices", nil)
	}
	cfg := rest.CopyConfig(l.target.Config)
	cfg.Wrap(func(next http.RoundTripper) http.RoundTripper { return contextTransport{ctx: ctx, next: next} })
	actions := &action.Configuration{}
	err := actions.Init(clusterGetter{config: cfg, namespace: namespace}, namespace, "secret", func(string, ...any) {})
	if err != nil {
		return nil, err
	}
	client, err := actions.KubernetesClientSet()
	if err != nil {
		return nil, err
	}
	actions.Releases.Driver = newReleaseSecrets(client.CoreV1().Secrets(namespace))
	return actions, nil
}

type contextTransport struct {
	ctx  context.Context
	next http.RoundTripper
}

func (t contextTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.next.RoundTrip(r.WithContext(t.ctx))
}

// clusterGetter never consults the process's default kubeconfig or disk cache.
type clusterGetter struct {
	config    *rest.Config
	namespace string
}

func (g clusterGetter) ToRESTConfig() (*rest.Config, error) { return rest.CopyConfig(g.config), nil }
func (g clusterGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	client, err := discovery.NewDiscoveryClientForConfig(g.config)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(client), nil
}
func (g clusterGetter) ToRESTMapper() (meta.RESTMapper, error) {
	client, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(client), nil
}
func (g clusterGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return clientcmd.NewDefaultClientConfig(clientcmdapi.Config{
		CurrentContext: "selected",
		Contexts:       map[string]*clientcmdapi.Context{"selected": {Namespace: g.namespace, Cluster: "selected"}},
		Clusters:       map[string]*clientcmdapi.Cluster{"selected": {Server: g.config.Host}},
	}, &clientcmd.ConfigOverrides{})
}
