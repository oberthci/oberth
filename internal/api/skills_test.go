package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/apicontract"
	"go.yaml.in/yaml/v3"
)

type manifest struct {
	URI         string
	Frontmatter map[string]any
	Resources   []struct {
		URI, Digest string
		Size        int
	}
}

func skillResult(t *testing.T, f *fixture, method string, params any) map[string]json.RawMessage {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	w := request(f, "POST", "/mcp", body)
	assertSuccess(t, w)
	var reply struct{ Result map[string]json.RawMessage }
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if string(reply.Result["resultType"]) != `"complete"` || string(reply.Result["ttlMs"]) != "0" || string(reply.Result["cacheScope"]) != `"public"` {
		t.Fatal("skill cache/discriminator changed", w.Body.String())
	}
	return reply.Result
}

func skillManifest(t *testing.T, f *fixture) manifest {
	t.Helper()
	var entries []manifest
	if err := json.Unmarshal(skillResult(t, f, "skills/list", nil)["skills"], &entries); err != nil || len(entries) != 1 {
		t.Fatal("expected one skill", err)
	}
	return entries[0]
}

func readSkillText(t *testing.T, f *fixture, uri string) string {
	t.Helper()
	var files []struct{ URI, MimeType, Text string }
	if err := json.Unmarshal(skillResult(t, f, "resources/read", map[string]string{"uri": uri})["contents"], &files); err != nil || len(files) != 1 || files[0].URI != uri || files[0].MimeType != "text/markdown" {
		t.Fatal("invalid skill resource", uri, err)
	}
	return files[0].Text
}

func servedSkillText(t *testing.T, f *fixture) string {
	t.Helper()
	var parts []string
	for _, file := range skillManifest(t, f).Resources {
		parts = append(parts, readSkillText(t, f, file.URI))
	}
	return strings.Join(parts, "\n")
}

func TestSkillManifest(t *testing.T) {
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) { c.Version = "v1.2.3" })
	entry := skillManifest(t, f)
	if entry.URI != "skill://oberth/v1.2.3/oberth-ci/SKILL.md" || len(entry.Resources) != 3 {
		t.Fatal("unversioned/incomplete manifest", entry)
	}
	var got manifest
	if json.Unmarshal(skillResult(t, f, "skills/get", map[string]string{"uri": entry.URI})["skill"], &got) != nil || !reflect.DeepEqual(got, entry) {
		t.Fatal("list/get differ")
	}
	listed := skillResult(t, f, "resources/list", nil)
	var resources []struct{ URI string }
	if json.Unmarshal(listed["resources"], &resources) != nil || len(resources) != 3 {
		t.Fatal("missing resource catalogue")
	}
	checkSkillContent(t, f, entry)
}

func checkSkillContent(t *testing.T, f *fixture, entry manifest) {
	t.Helper()
	seen := map[string]bool{}
	for _, file := range entry.Resources {
		text := readSkillText(t, f, file.URI)
		digest := sha256.Sum256([]byte(text))
		if seen[file.URI] || file.Size != len(text) || file.Digest != "sha256:"+hex.EncodeToString(digest[:]) {
			t.Fatal("manifest integrity mismatch", file.URI)
		}
		seen[file.URI] = true
	}
	checkSkillFrontmatter(t, f, entry, seen)
}

func checkSkillFrontmatter(t *testing.T, f *fixture, entry manifest, seen map[string]bool) {
	t.Helper()
	text := readSkillText(t, f, entry.URI)
	front := strings.Split(text, "---\n")[1]
	var parsed map[string]any
	if yaml.Unmarshal([]byte(front), &parsed) != nil || !reflect.DeepEqual(parsed, entry.Frontmatter) {
		t.Fatal("frontmatter differs from manifest")
	}
	if parsed["name"] != "oberth-ci" || parsed["metadata"].(map[string]any)["version"] != "v1.2.3" {
		t.Fatal("wrong skill identity")
	}
	for _, path := range []string{"references/fix-resume.md", "references/display.md"} {
		if !seen[strings.TrimSuffix(entry.URI, "SKILL.md")+path] || !strings.Contains(text, path) {
			t.Fatal("reference is outside manifest", path)
		}
	}
}

func TestSkillDiscoveryAndUpgrade(t *testing.T) {
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) { c.Version = "S1" })
	discovery := discover(f).Body.String()
	for _, phrase := range []string{"io.modelcontextprotocol/skills", "skill://oberth/S1/oberth-ci/SKILL.md", "2.1.331+", "resource read alone", "unassisted", "uncertain", "Forms grant no authority", "display blocks whole"} {
		if !strings.Contains(discovery, phrase) {
			t.Fatal("missing discovery invariant", phrase)
		}
	}
	if strings.Contains(discovery, "events_wait") || strings.Contains(discovery, "directoryRead") {
		t.Fatal("workflow eagerly loaded or directory reader advertised")
	}
	old := skillManifest(t, f)
	// Reads expose immutable content. Host activation and origin approval are
	// separate; the server never treats reading this public file as permission.
	_ = servedSkillText(t, f)
	checkNoSkillMutations(t, f)
	upgraded := setup(t, func(c *api.Config, _ *apicontract.Bindings) { c.Version = "S2" })
	current := skillManifest(t, upgraded)
	if old.URI == current.URI || old.Resources[0].Digest == current.Resources[0].Digest {
		t.Fatal("upgrade retained old identity")
	}
	checkOldSkillURIs(t, upgraded, old.URI, current.URI)
	text := servedSkillText(t, upgraded)
	for _, phrase := range []string{"originating server", "complete manifest", "hash or size mismatch", "explicitly reload", "valid calls cannot detect", "Zero discovery TTL"} {
		if !strings.Contains(text, phrase) {
			t.Fatal("upgrade/approval guard missing", phrase)
		}
	}
}

func checkNoSkillMutations(t *testing.T, f *fixture) {
	t.Helper()
	for _, d := range apicontract.Definitions(f.bindings) {
		if !d.Mutation {
			continue
		}
		field := ""
		for _, word := range strings.Split(d.Name, "_") {
			field += strings.ToUpper(word[:1]) + word[1:]
		}
		handler := reflect.ValueOf(f.bindings).FieldByName(field).FieldByName("Handler").Elem()
		if calls := handler.MethodByName("Observed").Call(nil)[2].Int(); calls != 0 {
			t.Fatal("skill read invoked mutation", d.Name, calls)
		}
	}
}

func checkOldSkillURIs(t *testing.T, f *fixture, oldURI, currentURI string) {
	t.Helper()
	for _, uri := range []string{"", oldURI, "skill://oberth/S2/oberth-ci/../SKILL.md", "skill://oberth/S2/oberth-ci/%2e%2e/SKILL.md", currentURI + "?old=1", strings.TrimSuffix(currentURI, "SKILL.md")} {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/read", "params": map[string]string{"uri": uri}})
		w := request(f, "POST", "/mcp", body)
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":-32602`) {
			t.Fatal("unknown URI accepted", uri, w.Body.String())
		}
	}
}

func TestSkillExamplesUseRegisteredToolsAndArguments(t *testing.T) {
	f := setup(t, nil)
	text := servedSkillText(t, f)
	count, err := checkSkillInvocations(f, text)
	if err != nil {
		t.Fatal(err)
	}
	if count < 10 {
		t.Fatal("tool examples disappeared")
	}
	for _, phrase := range []string{`mode:"retry"`, "failed R1 -> consented committed parked F", "Assisted R2", "fresh unassisted RF", "no same-Pod resume mode", "Do not request RF without that consent", "if it failed, fix and resume again", "never evidence"} {
		if !strings.Contains(text, phrase) {
			t.Fatal("retry-mode serial flow drifted", phrase)
		}
	}
	for _, future := range []string{"run_next {", "after_task", "resultType:\"task\""} {
		if strings.Contains(text, future) {
			t.Fatal("unshipped behavior advertised", future)
		}
	}
}

func checkSkillInvocations(f *fixture, text string) (int, error) {
	examples, err := skillInvocations(text)
	if err != nil {
		return 0, err
	}
	for _, example := range examples {
		w := rpc(f, example[0], json.RawMessage(example[1]))
		if w.Code != 200 || strings.Contains(w.Body.String(), `"isError":true`) || strings.Contains(w.Body.String(), `"error":`) {
			return 0, fmt.Errorf("invalid skill invocation %v: %s", example, w.Body.String())
		}
	}
	return len(examples), nil
}

func skillInvocations(text string) ([][2]string, error) {
	code := regexp.MustCompile("`[^`\n]+`")
	invocation := regexp.MustCompile(`^([a-z][a-z0-9_]*)\s+(.+)$`)
	var examples [][2]string
	for _, span := range code.FindAllString(text, -1) {
		example := invocation.FindStringSubmatch(strings.Trim(span, "`"))
		if example == nil && !strings.ContainsAny(span, "{}") {
			continue
		}
		if example != nil && example[1] == "git" {
			continue // Shell commands are checked by the agent command contract.
		}
		if example == nil || !strings.HasPrefix(example[2], "{") || !json.Valid([]byte(example[2])) {
			return nil, fmt.Errorf("unchecked skill invocation: %s", span)
		}
		examples = append(examples, [2]string{example[1], example[2]})
	}
	if strings.ContainsAny(code.ReplaceAllString(text, ""), "{}") {
		return nil, fmt.Errorf("unchecked skill invocation outside inline code")
	}
	return examples, nil
}

func TestSkillInvocationGuardRejectsDrift(t *testing.T) {
	f := setup(t, nil)
	text := servedSkillText(t, f)
	for _, tc := range []struct{ name, call, want string }{
		{"missing tool", "`imaginary {}`", "imaginary"},
		{"missing argument", "`events_wait {\"snapshot\":true,\"own\":true,\"imaginary\":true}`", "imaginary"},
		{"prose argument", "events_wait {snapshot:true, own:true, imaginary:true}", "outside inline code"},
		{"quoted pseudo JSON", "`events_wait {snapshot:true, own:true}`", "unchecked skill invocation"},
		{"non-object arguments", "`events_wait []`", "unchecked skill invocation"},
		{"missing closing quote", "`events_wait {\"snapshot\":true}", "outside inline code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The former prose-call location must obey the same guard as the examples.
			changed := strings.Replace(text, "Before pushing, take the immediate event snapshot shown below, without after_id.", "Before pushing, call "+tc.call+".", 1)
			if changed == text {
				t.Fatal("prose-call control did not change the served skill")
			}
			if _, err := checkSkillInvocations(f, changed); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("instruction drift was not rejected: %v", err)
			}
		})
	}
}
