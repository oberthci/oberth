package argojob

import "strings"

// A log observation adds attribution only. It never changes a verdict or
// authorizes replay of an arbitrary build command (which may have side effects
// or also contain an integrity failure).
type goProxyDiagnostic struct {
	reported bool
}

func (diagnostic *goProxyDiagnostic) observe(line, proxyURL string) string {
	if diagnostic.reported || proxyURL == "" ||
		!strings.Contains(line, `"`+strings.TrimRight(proxyURL, "/")+`/`) ||
		!strings.Contains(line, "connect: connection refused") {
		return ""
	}
	diagnostic.reported = true
	return "oberth: Go module proxy unavailable (connection refused); check the proxy listener, Service endpoints and network policy; the build command was not automatically retried"
}
