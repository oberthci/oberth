package website

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestStaticSiteContract(t *testing.T) {
	t.Parallel()

	index := readSiteFile(t, "public/index.html")
	headers := readSiteFile(t, "public/_headers")

	required := []string{
		"<title>Oberth — AI-native CI · satellite Git & CI authority</title>",
		`rel="canonical" href="https://oberth.ci/"`,
		"Local CI at agent speed.",
		"The audit chain is tamper-evident — including against the box it runs on — not tamper-proof.",
		"The visibility boundary is intentional and explicit",
	}
	for _, value := range required {
		if !strings.Contains(index, value) {
			t.Errorf("index is missing required product truth %q", value)
		}
	}

	for _, value := range []string{"10-50x", "80% of failures", "tamper-proof audit", "one-command install is available"} {
		if strings.Contains(strings.ToLower(index), strings.ToLower(value)) {
			t.Errorf("index contains unsupported marketing claim %q", value)
		}
	}

	if strings.Contains(index, "watch.oberth.ci") {
		t.Error("index references the retired watch.oberth.ci dashboard host")
	}
	if got := strings.Count(index, `href="https://oberth.ci/repos"`); got != 3 {
		t.Errorf("dashboard link count = %d, want 3", got)
	}

	assertLocalAssetsExist(t, index)
	assertJSONLDAndScriptPolicy(t, index, headers)

	var manifest map[string]any
	if err := json.Unmarshal([]byte(readSiteFile(t, "public/site.webmanifest")), &manifest); err != nil {
		t.Fatalf("decode site.webmanifest: %v", err)
	}
	if manifest["name"] != "Oberth" {
		t.Errorf("manifest name = %v, want Oberth", manifest["name"])
	}
}

func TestWranglerRoutesApexAndWWW(t *testing.T) {
	t.Parallel()

	var config struct {
		Routes []struct {
			Pattern      string `json:"pattern"`
			CustomDomain bool   `json:"custom_domain"`
		} `json:"routes"`
	}
	if err := json.Unmarshal([]byte(readSiteFile(t, "wrangler.jsonc")), &config); err != nil {
		t.Fatalf("decode wrangler.jsonc: %v", err)
	}

	routes := make(map[string]bool, len(config.Routes))
	for _, route := range config.Routes {
		routes[route.Pattern] = route.CustomDomain
	}
	for _, hostname := range []string{"oberth.ci", "www.oberth.ci"} {
		if !routes[hostname] {
			t.Errorf("missing custom-domain route for %s", hostname)
		}
	}
}

func assertLocalAssetsExist(t *testing.T, index string) {
	t.Helper()
	re := regexp.MustCompile(`(?:href|src)="(/[^"]+)"`)
	for _, match := range re.FindAllStringSubmatch(index, -1) {
		asset := strings.SplitN(match[1], "?", 2)[0]
		asset = strings.SplitN(asset, "#", 2)[0]
		if asset == "/" {
			continue
		}
		path := filepath.Join("public", filepath.FromSlash(strings.TrimPrefix(asset, "/")))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("local asset %q: %v", asset, err)
		}
	}
}

// assertJSONLDAndScriptPolicy proves the structured-data block is valid JSON
// and that the CSP stays strict without inline-script escape hatches: JSON-LD
// data blocks are non-executable, so `script-src 'self'` needs no hash, and
// every other script element must load from a same-origin src.
func assertJSONLDAndScriptPolicy(t *testing.T, index, headers string) {
	t.Helper()
	re := regexp.MustCompile(`<script type="application/ld\+json">\n([\s\S]*?)\n</script>`)
	match := re.FindStringSubmatch(index)
	if len(match) != 2 {
		t.Fatal("JSON-LD block not found")
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(match[1]), &document); err != nil {
		t.Fatalf("decode JSON-LD: %v", err)
	}
	if !strings.Contains(headers, "script-src 'self'") {
		t.Error("CSP does not restrict script-src to 'self'")
	}
	if strings.Contains(headers, "unsafe-inline") {
		t.Error("CSP allows unsafe-inline")
	}
	for _, tag := range regexp.MustCompile(`<script[^>]*>`).FindAllString(index, -1) {
		if strings.Contains(tag, `type="application/ld+json"`) || strings.Contains(tag, `src="`) {
			continue
		}
		t.Errorf("inline executable script violates script-src 'self': %s", tag)
	}
}

func readSiteFile(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}
