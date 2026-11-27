package api_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

type webWire struct {
	Path, Method, Body string
	Headers            map[string]string
	Refusal            struct {
		Reason, Remedy string
		Status         int
	}
}

func TestWebUIModernMCPWire(t *testing.T) {
	sent := captureWebWire(t)
	if sent.Refusal.Status != 400 || sent.Refusal.Reason == "" || sent.Refusal.Remedy == "" {
		t.Fatal("web UI treated a protocol refusal as a connection failure", sent.Refusal)
	}
	if sent.Path != "/mcp" || sent.Method != "POST" {
		t.Fatal("web client used another wire")
	}
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) { c.BrowserOrigin = "https://OBERTH.CI:443/mcp" })
	r := httptest.NewRequest(sent.Method, sent.Path, strings.NewReader(sent.Body))
	for name, value := range sent.Headers {
		r.Header.Set(name, value)
	}
	r.Header.Set("Authorization", "Bearer valid")
	r.Header.Set("Origin", "https://oberth.ci")
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, r)
	assertSuccess(t, w)
	h := f.bindings.RunCancel.Handler.(*fake.Handler[apicontract.RunCancelArgs, apicontract.Receipt])
	if inputs, _, calls := h.Observed(); len(inputs) != 1 || calls != 1 {
		t.Fatal("web action never reached its existing handler")
	}
}

func captureWebWire(t *testing.T) webWire {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	source, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(string(source))
	program := fmt.Sprintf(`const vm=require('node:vm');
const source=%s;
const box={document:{querySelector(){return {content:'web-build'}}},localStorage:{getItem(){return 'valid'}},reached(){}};
vm.createContext(box);
vm.runInContext(source.slice(0,source.indexOf('\nsignin.querySelector')),box);
const exchange=box.exchange;
let sent;
box.exchange=async(path,options)=>{sent={path,...options};return {json:async()=>({result:{resultType:'complete',structuredContent:{ok:true}}})}};
(async()=>{
 await box.call('run_cancel',{id:'11111111'});
 box.fetch=async()=>({ok:false,status:400,headers:{get(){return null}},json:async()=>({error:{code:-32022,message:'Unsupported protocol version',data:{supported:['2026-07-28']}}})});
 try {await exchange('/mcp')} catch(error) {sent.refusal={reason:error.reason,remedy:error.remedy,status:error.status}}
 process.stdout.write(JSON.stringify(sent));
})().catch(error=>{console.error(error);process.exitCode=1});`, encoded)
	out, err := exec.CommandContext(t.Context(), "node", "-e", program).CombinedOutput()
	if err != nil {
		t.Fatalf("web client: %v %s", err, out)
	}
	var sent webWire
	if err = json.Unmarshal(out, &sent); err != nil {
		t.Fatal(err, string(out))
	}
	return sent
}

func TestMCPOriginCannotFollowUntrustedHost(t *testing.T) {
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) { c.BrowserOrigin = "https://oberth.ci/mcp" })
	for _, origin := range []string{"null", "", "https://evil.example", "https://oberth.ci.evil.example"} {
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.Host = "evil.example"
		r.Body = unreadBody{t}
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("untrusted origin reached the body", origin, w.Code)
		}
	}
}
