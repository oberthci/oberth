package releasecredential

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModeFailsClosedBeforeLegacyRead(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("legacy-sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"1", "0", "true", "unexpected"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OBERTH_RELEASE_WIF", mode)
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/run/oberth-gar/adc.json")
			if _, err := FromEnvironment(context.Background(), token); err == nil {
				t.Fatal("WIF accepted legacy token operand")
			}
		})
	}
	t.Setenv("OBERTH_RELEASE_WIF", "")
	if _, err := FromEnvironment(context.Background(), "wif"); err == nil {
		t.Fatal("marker accepted without WIF")
	}
	a, err := FromEnvironment(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := a.Authorization()
	if err != nil || auth.Password != "legacy-sentinel" {
		t.Fatal("explicit rollback failed")
	}
	t.Setenv("OBERTH_RELEASE_WIF", "1")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/wrong")
	if _, err := FromEnvironment(context.Background(), "wif"); err == nil {
		t.Fatal("wrong ADC accepted")
	}
}

func TestHelperPipeAndRenewal(t *testing.T) {
	script := filepath.Join(t.TempDir(), "helper.py")
	body := `import json,os,stat,sys
assert sys.argv[1:] == ['get']
assert sys.stdin.read() == 'europe-west4-docker.pkg.dev\n'
assert stat.S_ISFIFO(os.fstat(1).st_mode)
assert os.environ['GOOGLE_APPLICATION_CREDENTIALS'] == '/run/oberth-gar/adc.json'
assert 'POISON' not in os.environ
print(json.dumps({'Username':'oauth2accesstoken','Secret':str(os.getpid())}))
`
	if err := os.WriteFile(script, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POISON", "secret")
	c := &credential{ctx: context.Background(), script: script}
	a, err := c.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	if a.Password == b.Password {
		t.Fatal("credential was cached across auth requests")
	}
	entries, err := os.ReadDir(filepath.Dir(script))
	if err != nil || len(entries) != 1 {
		t.Fatal("helper wrote a credential file")
	}
}

func TestHelperFailureNeverExposesOutputOrFallsBack(t *testing.T) {
	for _, body := range []string{
		"import sys; print('PRIVATE_SENTINEL'); print('PRIVATE_SENTINEL',file=sys.stderr); sys.exit(1)",
		"print('PRIVATE_SENTINEL')", "print('X'*65537)",
		`print('{"Username":"other","Secret":"PRIVATE_SENTINEL"}')`,
		`print('{"Username":"oauth2accesstoken","Secret":""}')`,
	} {
		t.Run(body[:min(len(body), 24)], func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "helper.py")
			if err := os.WriteFile(script, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			c := &credential{ctx: context.Background(), script: script}
			a, err := c.Authorization()
			if a != nil || err == nil || err.Error() != "keyless registry authentication failed" || strings.Contains(err.Error(), "PRIVATE_SENTINEL") {
				t.Fatalf("unsafe result %v", err)
			}
		})
	}
}
