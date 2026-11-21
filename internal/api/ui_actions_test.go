package api_test

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestUIActionsMakeOneTypedRequest(t *testing.T) {
	f := setup(t, nil)
	for _, name := range []string{"run_cancel", "run_rerun", "run_hold_abandon", "promote"} {
		args := map[string]any{"id": "12345678", "reason": "web UI"}
		switch name {
		case "promote":
			args = map[string]any{"repo": "origin/team/widget", "sha": strings.Repeat("a", 40), "reason": "web UI"}
		case "run_hold_abandon":
			args["step"] = "build"
		}
		assertSuccess(t, rpc(f, name, args))
		assertBindingReached(t, f.bindings, name)
	}
	rerun := f.bindings.RunRerun.Handler.(*fake.Handler[apicontract.RunRerunArgs, apicontract.Receipt])
	inputs, _, _ := rerun.Observed()
	if !reflect.DeepEqual(inputs, []apicontract.RunRerunArgs{{ID: "12345678", Reason: "web UI"}}) {
		t.Fatal("action arguments changed", inputs)
	}
}

func TestUICopyCatalogue(t *testing.T) {
	html, js, _ := uiSources(t)
	catalogue, _, ok := strings.Cut(js, "};")
	if !ok {
		t.Fatal("copy catalogue absent")
	}
	entries := regexp.MustCompile(`\w+: \[([^\]]+)\]`).FindAllStringSubmatch(catalogue, -1)
	if len(entries) < 12 {
		t.Fatal("incomplete copy catalogue")
	}
	for _, entry := range entries {
		for index, match := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(entry[1], -1) {
			limit := 80
			if index == 1 {
				limit = 120
			}
			checkUICopy(t, match[1], limit)
		}
	}
	for _, match := range regexp.MustCompile(`<p>([^<]+)</p>`).FindAllStringSubmatch(html, -1) {
		checkUICopy(t, match[1], 120)
	}
}
func checkUICopy(t *testing.T, text string, limit int) {
	t.Helper()
	if utf8.RuneCountInString(text) > limit || strings.Contains(text, "…") || strings.Contains(text, "...") || strings.Count(text, ".")+strings.Count(text, "?") != 1 {
		t.Fatal("client sentence exceeds copy contract", text)
	}
}
