package client

import (
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestClientRefusesQueryAndFragmentDelimiters(t *testing.T) {
	for _, suffix := range []string{"?", "#", "?q=x", "#fragment"} {
		if _, err := New(Config{Options: Options{URL: "https://ci.example/mcp" + suffix}, Transport: handlerTransport(server(t, fake.Bindings()))}); err == nil {
			t.Fatal("client accepted delimiter", suffix)
		}
	}
}

func TestClientBuildsEndpointFromParsedAddress(t *testing.T) {
	for _, tc := range []struct{ address, endpoint string }{
		{"https://ci.example", "https://ci.example/mcp"},
		{"https://ci.example/prefix/", "https://ci.example/prefix/mcp"},
		{"https://ci.example/a%2Fb/", "https://ci.example/a%2Fb/mcp"},
		{"https://ci.example/é", "https://ci.example/%C3%A9/mcp"},
	} {
		c, err := New(Config{Options: Options{URL: tc.address}, Transport: handlerTransport(server(t, fake.Bindings()))})
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		if c.endpoint != tc.endpoint {
			t.Fatal("endpoint differs", c.endpoint, tc.endpoint)
		}
	}
}
