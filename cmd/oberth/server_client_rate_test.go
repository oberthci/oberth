package main

import (
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

// The server's clients carry the named rate, not the library's 5 and 10.
func TestServerClusterClientsCarryTheNamedRate(t *testing.T) {
	t.Parallel()
	clients, err := newClusterClients(&rest.Config{Host: "https://cluster.oberth.ci"})
	if err != nil {
		t.Fatal(err)
	}
	for name, limiter := range map[string]flowcontrol.RateLimiter{"kubernetes": clients.Kube.CoreV1().RESTClient().GetRateLimiter(), "argo": clients.Argo.ArgoprojV1alpha1().RESTClient().GetRateLimiter()} {
		if limiter == nil || limiter.QPS() != serverClientQPS {
			t.Fatal(name, "client rate differs from serverClientQPS")
		}
		// Tokens only accumulate with time, so the whole burst is available at once.
		for i := range serverClientBurst {
			if !limiter.TryAccept() {
				t.Fatal(name, "client burst ended after", i, "requests")
			}
		}
	}
}
