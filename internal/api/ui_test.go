package api_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
)

const shellCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; require-trusted-types-for 'script'"

func uiRequest(f *fixture, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, r)
	return w
}

// uiScripts are the page's scripts, app.js first so its copy catalogue leads the sources.
func uiScripts() []string { return []string{"app.js", "log.js", "issues.js"} }

// Each script and the stylesheet stay under the 500-line file budget.
func TestUIAssetsStayWithinTheFileBudget(t *testing.T) {
	f := setup(t, nil)
	for _, name := range append(uiScripts(), "app.css") {
		if lines := strings.Count(uiRequest(f, "/assets/"+name).Body.String(), "\n"); lines == 0 || lines >= 500 {
			t.Fatal("asset outside the file budget", name, lines)
		}
	}
}
func uiSources(t *testing.T) (string, string, string) {
	t.Helper()
	f := setup(t, nil)
	scripts := ""
	for _, name := range uiScripts() {
		scripts += uiRequest(f, "/assets/"+name).Body.String()
	}
	return uiRequest(f, "/runs").Body.String(), scripts, uiRequest(f, "/assets/app.css").Body.String()
}
func TestUIShellPublicIdentical(t *testing.T) {
	f := setup(t, nil)
	expected := uiRequest(f, "/runs").Body.Bytes()
	for _, path := range []string{"/runs", "/runs/12345678", "/repos", "/issues", "/issues/42", "/status"} {
		w := uiRequest(f, path)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), expected) || w.Header().Get("Cache-Control") != "no-cache" {
			t.Fatal("shell differs", path, w.Code)
		}
	}
	if w := uiRequest(f, "/"); w.Code != 303 || w.Header().Get("Location") != "/runs" {
		t.Fatal("root redirect", w.Code)
	}
	if bytes.Contains(expected, []byte("12345678")) || bytes.Contains(expected, []byte("valid")) {
		t.Fatal("shell carries private data")
	}
}
func TestUIHeaders(t *testing.T) {
	f := setup(t, nil)
	for _, path := range []string{"/runs", "/runs/12345678", "/repos", "/issues/1", "/status"} {
		w := uiRequest(f, path)
		for key, value := range map[string]string{"Content-Security-Policy": shellCSP, "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer"} {
			if w.Header().Get(key) != value {
				t.Fatal("header differs", path, key, w.Header().Get(key))
			}
		}
	}
}
func TestUIAssetsVersioned(t *testing.T) {
	f := setup(t, nil)
	shell := uiRequest(f, "/runs").Body.String()
	for _, name := range append(uiScripts(), "app.css") {
		w := uiRequest(f, "/assets/"+name)
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("asset sniffing permitted", name)
		}
		sum := sha256.Sum256(w.Body.Bytes())
		url := fmt.Sprintf("/assets/%s?v=%x", name, sum[:8])
		if !strings.Contains(shell, url) || w.Header().Get("Cache-Control") != "no-cache" {
			t.Fatal("unversioned asset", name)
		}
		if got := uiRequest(f, url); got.Code != 200 || got.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Fatal("immutable version", name)
		}
		if got := uiRequest(f, "/assets/"+name+"?v=wrong"); got.Header().Get("Cache-Control") != "no-cache" {
			t.Fatal("unknown version cached")
		}
	}
	for _, path := range []string{"/assets/", "/assets/index.html", "/assets/nope", "/assets/app.js/extra", "/assets/%2e%2e", "/assets/%2e%2e%2fapp.js", "/assets/../app.js", "/assets/..", "/assets/."} {
		if w := uiRequest(f, path); w.Code != 404 {
			t.Fatal("non-map asset served", path, w.Code)
		}
	}
}
func TestUIAssetsOffline(t *testing.T) {
	html, js, css := uiSources(t)
	forbidden := regexp.MustCompile(`(?i)https?:|//[a-z]|@import|@font-face|\.woff|\.ttf`)
	for _, source := range []string{html, js, css} {
		if found := forbidden.FindString(source); found != "" {
			t.Fatal("external resource", found)
		}
	}
}

func TestUINoHTMLSinks(t *testing.T) {
	html, js, _ := uiSources(t)
	if regexp.MustCompile(`(?i)\s(?:style|on\w+)\s*=|<script[^>]*>\s*[^<\s]`).MatchString(html) {
		t.Fatal("inline script or style")
	}
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "Function("} {
		if strings.Contains(js, sink) {
			t.Fatal("HTML sink", sink)
		}
	}
	if !strings.Contains(js, "node.append(") || !strings.Contains(js, "textContent") {
		t.Fatal("text-node rendering absent")
	}
}
func TestUICallsDeclaredOperations(t *testing.T) {
	_, js, _ := uiSources(t)
	allowed := map[string]bool{"events_wait": true, "run_hold_list": true, "run_hold_abandon": true, "run_cancel": true, "run_rerun": true, "promote": true}
	for _, match := range regexp.MustCompile(`call\("([a-z_]+)"`).FindAllStringSubmatch(js, -1) {
		if !allowed[match[1]] {
			t.Fatal("undeclared or credential operation", match[1])
		}
		if _, ok := apicontract.Required(match[1]); !ok {
			t.Fatal("operation missing", match[1])
		}
	}
	routes := map[string]bool{"/api/runs": true, "/api/repos": true, "/api/issues": true, "/api/status": true}
	for _, match := range regexp.MustCompile(`"(/api/[^"?]*)`).FindAllStringSubmatch(js, -1) {
		path := strings.TrimSuffix(match[1], "/")
		if !routes[path] && path != "/api/runs/counts" {
			t.Fatal("undeclared GET", path)
		}
	}
	for _, forbidden := range []string{"EventSource", "token=", "certificate", "public_key"} {
		if strings.Contains(js, forbidden) {
			t.Fatal("credential surface", forbidden)
		}
	}
}
func jsonFields(typ reflect.Type, fields map[string]bool) {
	if typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		jsonFields(typ.Elem(), fields)
		return
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for index := range typ.NumField() {
		field := typ.Field(index)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name != "" {
			fields[name] = true
		}
		jsonFields(field.Type, fields)
	}
}
func TestUIFieldsAreContractFields(t *testing.T) {
	_, js, _ := uiSources(t)
	fields := map[string]bool{}
	for _, value := range []any{stepmodel.RunSummary{}, apicontract.RunPage{}, apicontract.RepoPage{}, apicontract.LogResult{}, logcontract.Meta{}, apicontract.IssuePage{}, model.Issue{}, apicontract.SystemStatus{}, apicontract.RunCounts{}, apicontract.HoldPage{}, apicontract.EventResult{}, apicontract.Receipt{}, apicontract.RunLogsArgs{}, apicontract.EventsWaitArgs{}} {
		jsonFields(reflect.TypeOf(value), fields)
	}
	for _, match := range regexp.MustCompile(`\.([a-z]+_[a-z_]+)\b`).FindAllStringSubmatch(js, -1) {
		if !fields[match[1]] {
			t.Fatal("invented response field", match[1])
		}
	}
}

func TestUINeverTruncates(t *testing.T) {
	html, js, css := uiSources(t)
	for _, source := range []string{html, js, css} {
		if strings.Contains(source, "line-clamp") || strings.Contains(source, "…") || regexp.MustCompile(`["'][^"'\n]*\.\.\.["']`).MatchString(source) {
			t.Fatal("truncation or a truncated literal")
		}
	}
	for _, source := range []string{html, js} {
		if regexp.MustCompile(`ellipsis|overflow:\s*hidden`).MatchString(source) {
			t.Fatal("truncation outside the stylesheet")
		}
	}
	if selector := clippingOutsideNames(css); selector != "" {
		t.Fatal("CSS clips text outside names and issue titles", selector)
	}
	if regexp.MustCompile(`(?i)(?:max-height|line-clamp)\s*:`).MatchString(css) {
		t.Fatal("CSS clips text")
	}
	for _, room := range []string{"minmax(calc(40ch", ".runs .row > .message { white-space: normal; }", "pre-wrap", "overflow-wrap: anywhere"} {
		if !strings.Contains(css, room) {
			t.Fatal("full reasons/logs cannot wrap", room)
		}
	}
}

// clippingOutsideNames returns the first CSS selector that clips text but is not
// a repository or ref name, or an issue title, in a list cell (each carries its whole
// name as a title). Reasons, messages and logs are never cut.
func clippingOutsideNames(css string) string {
	names := map[string]bool{".runs .row > .repo": true, ".runs .row > .ref": true, ".repo-filter": true, ".repo-group": true, ".issues .row > .title": true}
	rules := regexp.MustCompile(`/\*[\s\S]*?\*/`).ReplaceAllString(css, "")
	clips := regexp.MustCompile(`(?i)text-overflow|overflow(?:-[xy])?\s*:\s*(?:clip|hidden)\b`)
	for _, rule := range regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`).FindAllStringSubmatch(rules, -1) {
		if !clips.MatchString(rule[2]) {
			continue
		}
		for _, selector := range strings.Split(rule[1], ",") {
			if !names[strings.TrimSpace(selector)] {
				return strings.TrimSpace(selector)
			}
		}
	}
	return ""
}
func TestUINoPollingTimers(t *testing.T) {
	_, js, _ := uiSources(t)
	if strings.Count(js, "setInterval(") != 1 || strings.Count(js, "setTimeout(") != 1 || !strings.Contains(js, "setTimeout(finish, milliseconds)") {
		t.Fatal("polling timer")
	}
	if strings.Contains(js, "http-equiv") || !strings.Contains(js, `call("events_wait"`) {
		t.Fatal("live observation absent")
	}
}
func TestUIRoutesDoNotShadowAPI(t *testing.T) {
	f := setup(t, nil)
	for _, path := range []string{"/api/unknown", "/assets/x", "/mcp/unknown", "/unknown"} {
		if w := uiRequest(f, path); w.Code != 404 || strings.Contains(w.Body.String(), "<!doctype") {
			t.Fatal("shell masks route", path, w.Code)
		}
	}
	if w := uiRequest(f, "/mcp"); w.Code != 401 {
		t.Fatal("MCP authentication bypassed", w.Code)
	}
}
