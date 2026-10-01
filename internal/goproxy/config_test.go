package goproxy

import (
	"net/http"
	"testing"
)

func TestNamespaceConfigurationFailsClosed(t *testing.T) {
	good := NamespaceConfig{ModulePrefix: "go.example.test", RepositoryPrefix: "sample-", Upstream: "forge", Organization: "example"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"", "*.example.test", "go.example.test/", "go.example.test/../public", "go.example.test?x", "go.example.test\\public"} {
		c := good
		c.ModulePrefix = prefix
		if err := c.Validate(); err == nil {
			t.Fatalf("invalid prefix %q accepted", prefix)
		}
		assertRoutingStatus(t, NewHandler(&mockCache{}, prefix, nil), "/public.example.test/module/@v/list", http.StatusForbidden)
	}
	for _, owner := range []string{"", "../other", "forge/example", "*"} {
		c := good
		c.Upstream = owner
		if err := c.Validate(); err == nil {
			t.Fatalf("invalid owner %q accepted", owner)
		}
	}
}

func TestHandlerSnapshotsConfiguredRoutes(t *testing.T) {
	routes := map[string]string{"wire": "forge/example/sample-wire"}
	h := NewHandler(&mockCache{}, "go.example.test", routes)
	routes["untrusted"] = "other/untrusted/source"
	assertRoutingStatus(t, h, "/go.example.test/untrusted/@v/list", http.StatusForbidden)
	assertRoutingStatus(t, h, "/go.example.test.elsewhere/module/@v/list", http.StatusNotFound)
}
