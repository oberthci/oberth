package installer

import (
	"context"
	"errors"
	"io"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"net/http"
	"strconv"

	"github.com/oberthci/oberth/internal/secretstore"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

func (e *Engine) storePod(ctx context.Context, namespace string) (string, error) {
	sts, err := e.Cluster.Client.AppsV1().StatefulSets(namespace).Get(ctx, "oberth-store", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", problem("store-starting", err)
	}
	if err != nil {
		return "", problem("cluster", err)
	}
	if !ownedStore(sts, namespace) {
		return "", problem("store-pod", nil)
	}
	selector, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
	if err != nil {
		return "", problem("store-pod", err)
	}
	pods, err := e.Cluster.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return "", problem("cluster", err)
	}
	for _, pod := range pods.Items {
		if metav1.IsControlledBy(&pod, sts) && pod.DeletionTimestamp == nil && storeReady(pod) {
			return pod.Name, nil
		}
	}
	return "", problem("store-starting", nil)
}
func (e *Engine) forwardStore(ctx context.Context, cfg secretstore.Config, ca []byte) (*secretstore.Client, func(), error) {
	pod, err := e.waitStorePod(ctx, cfg.ServerNamespace)
	if err != nil {
		return nil, nil, err
	}
	transport, upgrader, err := spdy.RoundTripperFor(e.Cluster.Config)
	if err != nil {
		return nil, nil, problem("cluster", err)
	}
	endpoint := e.Cluster.Client.CoreV1().RESTClient().Post().Namespace(cfg.ServerNamespace).Resource("pods").Name(pod).SubResource("portforward").URL()
	lifetime, cancel := context.WithCancel(ctx)
	client := &http.Client{Transport: contextTransport{ctx: lifetime, next: transport}}
	dialer := spdy.NewDialer(upgrader, client, http.MethodPost, endpoint)
	ready, done := make(chan struct{}), make(chan struct{})
	forward, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{"0:8200"}, lifetime.Done(), ready, io.Discard, io.Discard)
	if err != nil {
		cancel()
		return nil, nil, problem("cluster", err)
	}
	var forwardErr error
	go func() { defer close(done); forwardErr = forward.ForwardPorts() }()
	closeForward := func() { cancel(); <-done }
	select {
	case <-ready:
	case <-done:
		cancel()
		return nil, nil, problem("cluster", forwardErr)
	case <-ctx.Done():
		closeForward()
		return nil, nil, problem("cluster", ctx.Err())
	}
	ports, err := forward.GetPorts()
	if err != nil || len(ports) != 1 {
		closeForward()
		return nil, nil, problem("cluster", err)
	}
	store, err := secretstore.New(secretstore.Connection{URL: "https://127.0.0.1:" + strconv.Itoa(int(ports[0].Local)), ServerName: storeDNS(cfg.ServerNamespace), CA: ca})
	if err != nil {
		closeForward()
		return nil, nil, err
	}
	return store, closeForward, nil
}

func (e *Engine) waitStorePod(ctx context.Context, namespace string) (string, error) {
	for {
		pod, err := e.storePod(ctx, namespace)
		var p *Problem
		if !errors.As(err, &p) || p.Code != "store-starting" {
			return pod, err
		}
		if err = pauseReady(ctx); err != nil {
			return "", problem("timeout", err)
		}
	}
}

func ownedStore(sts *appsv1.StatefulSet, namespace string) bool {
	return sts.UID != "" && sts.Spec.Selector != nil && sts.Annotations["meta.helm.sh/release-name"] == "oberth" && sts.Annotations["meta.helm.sh/release-namespace"] == namespace
}

func storeReady(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
