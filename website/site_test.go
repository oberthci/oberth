package website

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
		"AI agents produce code at machine speed",
		"Tamper-evident",
		"The visibility boundary is intentional and explicit",
		// The golden path and the hosted secret-store setup script are part of
		// the product surface: the one-liner must reference the exact asset
		// this site serves, and Helm commands must name the registry that
		// actually hosts the chart.
		"https://oberth.ci/setup-secretstore.sh",
		"https://charts.cloudtaser.io/oberth",
		"Secret store",
		"secretstore.enabled=true",
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

	// charts.oberth.ci does not resolve; watch.oberth.ci was retired with the
	// cloudflared dashboard subsystem. Neither may reappear in install copy.
	for _, brokenURL := range []string{"https://charts.oberth.ci", "https://watch.oberth.ci", "https://oberth.ci/watch"} {
		if strings.Contains(index, brokenURL) {
			t.Errorf("index contains retired or unresolvable URL %q", brokenURL)
		}
	}

	assertLocalAssetsExist(t, index)
	assertJSONLDDataBlock(t, index)
	assertCSPHasNoInlineScript(t, headers)

	var manifest map[string]any
	if err := json.Unmarshal([]byte(readSiteFile(t, "public/site.webmanifest")), &manifest); err != nil {
		t.Fatalf("decode site.webmanifest: %v", err)
	}
	if manifest["name"] != "Oberth" {
		t.Errorf("manifest name = %v, want Oberth", manifest["name"])
	}
}

// TestTryTLDRCard pins the #/try TLDR card: commands-only quickstart at the
// top of the page, wired into the existing .cp copy delegation (CSP forbids
// inline handlers, so the button must ride a .code container).
func TestTryTLDRCard(t *testing.T) {
	t.Parallel()

	index := readSiteFile(t, "public/index.html")

	card := regexp.MustCompile(`(?s)<div class="code tldr rv">.*?</div>\n  </div>`).FindString(index)
	if card == "" {
		t.Fatal("index has no TLDR card with class \"code tldr rv\" — COPY ALL depends on the .code class for the .cp delegation")
	}

	for _, value := range []string{
		"TLDR — paste and go",
		`<button class="cp">COPY ALL</button>`,
		"curl -sfL https://get.k3s.io | sh -s - --disable=traefik --write-kubeconfig-mode 644",
		"helm repo add openbao https://openbao.github.io/openbao-helm",
		"helm repo add oberth https://charts.cloudtaser.io/oberth",
		"helm repo update",
		"helm install openbao openbao/openbao -n openbao --create-namespace --set server.dev.enabled=true --set injector.enabled=false",
		"kubectl port-forward -n openbao svc/openbao 8200:8200 &amp; export BAO_ADDR=http://localhost:8200 BAO_TOKEN=root &amp;&amp; curl -fsSL https://oberth.ci/setup-secretstore.sh | bash -s -- --address http://localhost:8200",
		"helm install oberth oberth/oberth -n oberth --create-namespace --set secretstore.enabled=true --set secretstore.insecureHTTPForDev=true",
		"Scroll down for the guided walkthrough",
	} {
		if !strings.Contains(card, value) {
			t.Errorf("TLDR card is missing %q", value)
		}
	}

	// The card must hold exactly the seven golden-path commands — no prose
	// lines inside the pre.
	pre := regexp.MustCompile(`(?s)<pre>(.*?)</pre>`).FindStringSubmatch(card)
	if pre == nil {
		t.Fatal("TLDR card has no <pre> block")
	}
	if lines := len(strings.Split(strings.TrimSpace(pre[1]), "\n")); lines != 7 {
		t.Errorf("TLDR pre holds %d lines, want exactly 7 commands", lines)
	}
}

func TestSetupSecretstoreScriptIsServed(t *testing.T) {
	t.Parallel()

	script := readSiteFile(t, "public/setup-secretstore.sh")
	if !strings.HasPrefix(script, "#!/usr/bin/env bash") {
		t.Error("setup-secretstore.sh must start with a bash shebang")
	}
	for _, value := range []string{"set -euo pipefail", "--address", "--dry-run", "secretstore.enabled=true"} {
		if !strings.Contains(script, value) {
			t.Errorf("setup-secretstore.sh is missing %q", value)
		}
	}

	headers := readSiteFile(t, "public/_headers")
	section := regexp.MustCompile(`(?m)^/setup-secretstore\.sh\n((?:  .+\n?)+)`).FindStringSubmatch(headers)
	if section == nil {
		t.Fatal("_headers has no /setup-secretstore.sh section")
	}
	if !strings.Contains(section[1], "Content-Type: text/plain") {
		t.Error("_headers must serve setup-secretstore.sh as text/plain")
	}
}

// TestSchematicChipsHaveWriteUps pins the index.html ↔ app.js contract: every
// schematic chip index resolves to a FEATURES write-up and a PAGEMAP page.
func TestSchematicChipsHaveWriteUps(t *testing.T) {
	t.Parallel()

	index := readSiteFile(t, "public/index.html")
	app := readSiteFile(t, "public/app.js")

	features := len(regexp.MustCompile(`(?m)^ \{t:'`).FindAllString(app, -1))
	if features == 0 {
		t.Fatal("no FEATURES entries found in app.js")
	}
	pagemap := regexp.MustCompile(`var PAGEMAP=\[([^\]]*)\]`).FindStringSubmatch(app)
	if pagemap == nil {
		t.Fatal("no PAGEMAP found in app.js")
	}
	pages := len(strings.Split(pagemap[1], ","))
	if pages != features {
		t.Errorf("PAGEMAP has %d entries, FEATURES has %d — chips would route nowhere", pages, features)
	}

	for _, match := range regexp.MustCompile(`data-f="(\d+)"`).FindAllStringSubmatch(index, -1) {
		chip, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("chip index %q: %v", match[1], err)
		}
		if chip >= features {
			t.Errorf("chip data-f=%d has no FEATURES write-up (have %d)", chip, features)
		}
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

// assertJSONLDDataBlock proves the structured-data block is valid JSON. It is
// a non-executable data block, so CSP script-src does not apply to it; the
// executable-script posture is pinned by assertCSPHasNoInlineScript.
func assertJSONLDDataBlock(t *testing.T, index string) {
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
	if document["name"] != "Oberth" {
		t.Errorf("JSON-LD name = %v, want Oberth", document["name"])
	}
}

func assertCSPHasNoInlineScript(t *testing.T, headers string) {
	t.Helper()
	if !strings.Contains(headers, "Content-Security-Policy:") {
		t.Fatal("_headers has no Content-Security-Policy")
	}
	if !strings.Contains(headers, "script-src 'self'") {
		t.Error("CSP must restrict script-src to 'self'")
	}
	if strings.Contains(headers, "unsafe-inline") || strings.Contains(headers, "unsafe-eval") {
		t.Error("CSP must not allow unsafe-inline or unsafe-eval")
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
