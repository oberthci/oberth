package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/oberthci/oberth/internal/installer"
)

// Idle-wait fixtures have an external Argo controller with compliant live
// defaults, even when the Oberth Pod policy is disabled.
func upgradeWaitArgoObjects() []runtime.Object {
	return []runtime.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "workflow-controller", Namespace: installer.DefaultArgoNamespace}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "workflow-controller", Command: []string{"workflow-controller"}, Args: []string{"--configmap", "idle-test-config"}}}}}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "idle-test-config", Namespace: installer.DefaultArgoNamespace}, Data: map[string]string{"config": `{
			"mainContainer":{"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}},
			"executor":{"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}}
		}`}},
	}
}

func idleResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"X-Oberth-Idle-Version": {"1"}}, Body: io.NopCloser(strings.NewReader(body))}
}

type idleBrokenBody struct{}

func (idleBrokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (idleBrokenBody) Close() error             { return nil }

func TestWaitIdleTransientFailuresBackOffAndRecover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var attempts []time.Duration
		client := &http.Client{Transport: idleRoundTrip(func(*http.Request) (*http.Response, error) {
			attempts = append(attempts, time.Since(start))
			switch len(attempts) {
			case 1:
				return nil, errors.New("connection refused")
			case 2:
				return idleResponse(503, "unavailable"), nil
			case 3:
				response := idleResponse(409, "")
				response.Body = idleBrokenBody{}
				return response, nil
			case 4:
				return idleResponse(409, `{"version":1,"queued":1,"tag":1}`), nil
			case 5:
				return nil, errors.New("connection reset")
			default:
				return idleResponse(204, ""), nil
			}
		})}
		var output bytes.Buffer
		if err := waitIdle(t.Context(), client, "http://fixture", time.Second, idleWaitOptions{mode: "critical", output: &output}); err != nil {
			t.Fatal(err)
		}
		want := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second, 8 * time.Second, 9 * time.Second}
		if !slices.Equal(attempts, want) || strings.Count(output.String(), "transient idle probe failure:") != 1 {
			t.Fatalf("attempts=%v output=%s", attempts, &output)
		}
	})
}

func TestWaitIdleProtocolErrorsFailImmediately(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		body string
	}{
		{"auth", 401, ""}, {"forbidden", 403, ""}, {"not found", 404, ""}, {"bad request", 400, ""},
		{"invalid JSON", 409, "{"}, {"invalid schema", 409, `{"version":2}`}, {"missing snapshot", 409, ""},
		{"oversized", 409, strings.Repeat(" ", 4097)},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: idleRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				return idleResponse(test.code, test.body), nil
			})}
			if err := waitIdle(t.Context(), client, "http://fixture", time.Second, idleWaitOptions{mode: "critical"}); err == nil || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestWaitIdleCancellationInterruptsBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(500*time.Millisecond, cancel)
		cause := errors.New("fixture dial failed")
		client := &http.Client{Transport: idleRoundTrip(func(*http.Request) (*http.Response, error) { return nil, cause })}
		start := time.Now()
		err := waitIdle(ctx, client, "http://fixture", time.Second)
		if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) || time.Since(start) != 500*time.Millisecond {
			t.Fatalf("elapsed=%s err=%v", time.Since(start), err)
		}
	})
}

type idleDrainBody struct {
	data   *strings.Reader
	read   int
	closed bool
}

func (b *idleDrainBody) Read(p []byte) (int, error) {
	n, err := b.data.Read(p)
	b.read += n
	return n, err
}
func (b *idleDrainBody) Close() error { b.closed = true; return nil }

func TestIdleProbeDrainsServerErrorBodyBeforeClose(t *testing.T) {
	for _, size := range []int{64, 8192} {
		body := &idleDrainBody{data: strings.NewReader(strings.Repeat("x", size))}
		client := &http.Client{Transport: idleRoundTrip(func(*http.Request) (*http.Response, error) {
			response := idleResponse(503, "")
			response.Body = body
			return response, nil
		})}
		_, _, _, transient, err := probeIdle(t.Context(), client, "http://fixture", "critical")
		if err == nil || !transient || !body.closed || body.read != min(size, 4097) {
			t.Fatalf("size=%d read=%d closed=%t transient=%t err=%v", size, body.read, body.closed, transient, err)
		}
	}
}
