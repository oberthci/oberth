package apicontract

import "testing"

func TestSameServerAddress(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"https://ci.example", "https://CI.EXAMPLE:443/", true},
		{"https://ci.example", "https://ci.example/mcp", true},
		{"https://ci.example/prefix", "https://ci.example/prefix/mcp/", true},
		{"https://ci.example/a%2Fb", "https://ci.example/a%2Fb/mcp", true},
		{"https://ci.example/%6Dcp", "https://ci.example/mcp", false},
		{"https://ci.example/mcp/", "https://ci.example:443/mcp", true},
		{"https://[::1]/mcp", "https://[::1]:443/mcp/", true},
		{"https://ci.example:34443/mcp", "https://CI.EXAMPLE:34443/mcp", true},
		{"https://ci.example", "http://ci.example", false},
		{"https://ci.example", "https://ci.example:444", false},
		{"https://ci.example/mcp", "https://ci.example/other", false},
		{"https://ci.example/mcp", "https://ci.example/MCP", false},
		{"https://ci.example/a%2Fb", "https://ci.example/a/b", false},
		{"https://ci.example", "https://ci.example.", false},
		{"https://kube.local", "https://\u212Aube.local", false},
		{"https://host.local", "https://ho\u017Ft.local", false},
		{"https://bücher.example", "https://bücher.example", false},
		{"https://xn--bcher-kva.example", "https://XN--BCHER-KVA.example", true},
		{"https://ci.example", "://", false},
		{"https://ci.example/mcp?", "https://ci.example/mcp", false},
		{"https://ci.example/mcp#", "https://ci.example/mcp", false},
		{"https://ci.example/mcp?", "https://ci.example/mcp?", false},
		{"https://ci.example/mcp#", "https://ci.example/mcp#", false},
		{"", "", false},
	} {
		if got := SameServerAddress(tc.a, tc.b); got != tc.same || SameServerAddress(tc.b, tc.a) != tc.same {
			t.Errorf("%q / %q: got %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}

func TestASCIIHost(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"ci.example", true}, {"127.0.0.1", true}, {"::1", true},
		{"xn--bcher-kva.example", true}, {"", false}, {"bücher.example", false},
		{"\u212Aube.local", false}, {"ho\u017Ft.local", false},
	} {
		if ASCIIHost(tc.host) != tc.want {
			t.Error("ASCII host check differs", tc.host)
		}
	}
}
