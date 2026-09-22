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
		"<title>Oberth — Put your CI where your agent is</title>",
		`rel="canonical" href="https://oberth.ci/"`,
		"Red never leaves the box",
		"Oberth runs full CI on your hardware before every push syncs upstream",
		"Turn your own hardware into CI",
		"Tamper-evident",
		"The visibility boundary is intentional and explicit",
		// Helm commands must name the registry that actually hosts the chart.
		// The install golden path itself is the CLI setup wizard, pinned
		// against app.js by TestSetupWizardGoldenPath.
		"https://charts.cloudtaser.io/oberth",
		"Secret store",
		"Apache 2.0",
	}
	for _, value := range required {
		if !strings.Contains(index, value) {
			t.Errorf("index is missing required product truth %q", value)
		}
	}

	// The last four are the pre-2026-08 anti-Argo absolutes. Argo Workflow
	// authoring is now an accepted (planned, unshipped) on-ramp, so these
	// claims are retired and must not resurface via stale copy.
	for _, value := range []string{
		"10-50x", "80% of failures", "tamper-proof audit", "one-command install is available",
		"zero argo", "not even argo", "no dependency on argo", "not coming back",
	} {
		if strings.Contains(strings.ToLower(index), strings.ToLower(value)) {
			t.Errorf("index contains unsupported marketing claim %q", value)
		}
	}

	// charts.oberth.ci and releases.oberth.ci do not resolve; watch.oberth.ci
	// was retired with the cloudflared dashboard subsystem. None may reappear
	// in install copy — binaries and charts live on releases/charts.cloudtaser.io.
	for _, brokenURL := range []string{"https://charts.oberth.ci", "https://releases.oberth.ci", "https://watch.oberth.ci", "https://oberth.ci/watch"} {
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
	description, _ := manifest["description"].(string)
	if strings.Contains(description, "Go pipeline") {
		t.Errorf("manifest description contains retired 'Go pipeline' vocabulary: %s", description)
	}
	if !strings.Contains(description, "Argo Workflow") {
		t.Errorf("manifest description does not describe the live Argo Workflow pipeline path: %s", description)
	}

	// Prograde/Retrograde are retired burn names; the simulation captions must
	// use current vocabulary (green/red) instead (issue #53 item 4).
	for _, retired := range []string{"Prograde at periapsis", "Retrograde at periapsis"} {
		if strings.Contains(index, retired) {
			t.Errorf("index contains retired burn vocabulary %q — use green/red", retired)
		}
	}
}

// TestPeriapsisShowcase is removed: the periapsis-showcase section was deleted
// from index.html because the Go-DSL pipeline format it demonstrated is no
// longer Oberth's own dogfooded pipeline (see .oberth/build.yaml). periapsis.go
// itself is not retired as a product feature — it remains the required format
// for release-credentialed pipelines in every other repo that has not adopted
// the Argo path — but this page's flagship PIPELINE section now demonstrates
// what Oberth's own CI actually runs, and a periapsis.go code showcase
// alongside that would contradict it.

// TestNoInterpreterEraVocabulary pins out the retired yaegi/interpreter
// description of how a pipeline executes, and the now-inaccurate claim that
// the pipeline is Go orchestrating plain subprocesses rather than containers.
// Oberth's own pipeline is Argo Workflow YAML: each DAG task is its own
// container with its own exit code, not a subprocess of one Go program.
func TestNoInterpreterEraVocabulary(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"public/index.html", "public/app.js"} {
		body := strings.ToLower(readSiteFile(t, name))
		for _, retired := range []string{
			"yaegi",
			"interprets it",
			"interpreted by",
			"runner interprets",
			// Caught only on a second sweep: the Dagger comparison described
			// the pipeline as "interpreted Go" without ever saying yaegi, so a
			// grep for the interpreter's name missed it.
			"interpreted go",
			"orchestrating plain subprocesses",
		} {
			if strings.Contains(body, retired) {
				t.Errorf("%s describes the retired subprocess/interpreter era with %q", name, retired)
			}
		}
	}
}

// TestSetupWizardGoldenPath pins the setup wizard: the static skeleton in
// index.html carries the mount points, app.js carries one complete combo per
// platform+flavor with an `oberth install` golden path, and every generated
// TLDR block rides a .code container so the .cp COPY ALL delegation works
// under the no-inline-script CSP.
func TestSetupWizardGoldenPath(t *testing.T) {
	t.Parallel()

	index := readSiteFile(t, "public/index.html")
	app := readSiteFile(t, "public/app.js")

	// Static mount points the wizard JS renders into.
	for _, value := range []string{
		`id="setupWizard"`,
		`id="k8sFlavorGrid"`,
		`id="wizInstrBody"`,
		`data-platform="mac"`,
		`data-platform="linux"`,
	} {
		if !strings.Contains(index, value) {
			t.Errorf("index is missing setup wizard mount point %q", value)
		}
	}

	// The retired #/try route must keep resolving to the wizard.
	if !strings.Contains(app, `if(h==='try')h='setup'`) {
		t.Error("app.js no longer redirects #/try to #/setup")
	}

	// One complete combo per supported platform+flavor, each ending in the
	// CLI golden path. The dev secret store is the explicitly-labelled
	// evaluation mode.
	for _, combo := range []string{"'mac|kind'", "'linux|k3s'", "'linux|k0s'"} {
		if !strings.Contains(app, combo) {
			t.Errorf("app.js SETUP_COMBOS is missing combo %s", combo)
		}
	}
	for _, value := range []string{
		"oberth setup",
		"brew install oberthci/tap/oberth",
		"curl -fsSL https://oberth.ci/install.sh | sh",
		"https://get.k3s.io",
		"https://get.k0s.sh",
	} {
		if !strings.Contains(app, value) {
			t.Errorf("app.js setup wizard is missing golden-path command %q", value)
		}
	}

	// Cosign verification guidance must appear in the advanced section (issue #84).
	for _, value := range []string{
		"cosign verify-blob --key cosign.pub --insecure-ignore-tlog --bundle SHA256SUMS.sigstore.json SHA256SUMS",
		"SHA256SUMS.sigstore.json",
		"cosign.pub",
	} {
		if !strings.Contains(index, value) {
			t.Errorf("index.html advanced section is missing cosign verification guidance %q", value)
		}
	}
	if occurrences := strings.Count(app, "oberth setup"); occurrences < 3 {
		t.Errorf("only %d combos install via the CLI golden path, want every combo (3+)", occurrences)
	}

	// COPY ALL mechanics: the TLDR block is generated with the .code class and
	// the delegation resolves the pre through .closest('.code') — CSP forbids
	// inline handlers, so this is the only wiring that can work.
	for _, value := range []string{
		`<div class="code tldr">`,
		`<button class="cp">COPY ALL</button>`,
		"TLDR — paste and go",
		`closest('.code')`,
	} {
		if !strings.Contains(app, value) {
			t.Errorf("app.js setup wizard is missing copy-delegation wiring %q", value)
		}
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
// TestSetupSecretstoreScriptRelationship pins the relationship between the
// server-embedded canonical script (scripts/setup-secretstore.sh) and the
// public-facing installer variant (website/public/setup-secretstore.sh).
// The website copy adds Argo credentialed-tier setup (sections 7-8); both
// must carry RELATIONSHIP header comments and share the same security
// posture (read_field error classification, KUBERNETES_HOST validation).
func TestSetupSecretstoreScriptRelationship(t *testing.T) {
	t.Parallel()

	canonical, err := os.ReadFile("../scripts/setup-secretstore.sh")
	if err != nil {
		t.Fatalf("read scripts/setup-secretstore.sh: %v", err)
	}
	publicVariant := readSiteFile(t, "public/setup-secretstore.sh")

	// Both must declare the relationship.
	if !strings.Contains(string(canonical), "RELATIONSHIP:") {
		t.Error("scripts/setup-secretstore.sh missing RELATIONSHIP header comment")
	}
	if !strings.Contains(publicVariant, "RELATIONSHIP:") {
		t.Error("website/public/setup-secretstore.sh missing RELATIONSHIP header comment")
	}

	// The public variant must contain the Argo credentialed-tier sections.
	if !strings.Contains(publicVariant, "CREDENTIALED_SERVICE_ACCOUNT") {
		t.Error("website/public/setup-secretstore.sh is missing the Argo credentialed-tier setup")
	}

	// Both must have KUBERNETES_HOST HTTPS validation (security hardening).
	for name, content := range map[string]string{
		"scripts/setup-secretstore.sh":        string(canonical),
		"website/public/setup-secretstore.sh": publicVariant,
	} {
		if !strings.Contains(content, "KUBERNETES_HOST") || !strings.Contains(content, "plain HTTP") {
			t.Errorf("%s is missing KUBERNETES_HOST HTTPS validation", name)
		}
	}

	// Both must have robust read_field error handling.
	for name, content := range map[string]string{
		"scripts/setup-secretstore.sh":        string(canonical),
		"website/public/setup-secretstore.sh": publicVariant,
	} {
		if !strings.Contains(content, "permission denied") {
			t.Errorf("%s read_field does not classify permission errors", name)
		}
	}
}

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
