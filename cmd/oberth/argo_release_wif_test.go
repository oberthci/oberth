package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
)

func TestReadArgoReleaseWIFConfigEnabled(t *testing.T) {
	policy := argojob.ReleaseWIFConfig{
		Namespace: "oberth-argo",
		Roles: map[string]argojob.ReleaseWIFRole{"image-writer": {
			Provider:       "projects/123/locations/global/workloadIdentityPools/images/providers/issuer",
			ServiceAccount: "publisher@example-project.iam.gserviceaccount.com",
		}},
		Repositories: map[string]argojob.ReleaseWIFRepository{"github/org/repo": {
			ServiceAccountName: "repo-release", Templates: map[string]string{"publish": "image-writer"},
		}},
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "capabilities.json")
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	config, err := readArgoReleaseWIFConfig(name)
	if err != nil {
		t.Fatal(err)
	}
	if config.Namespace != policy.Namespace || config.Repositories["github/org/repo"].Templates["publish"] != "image-writer" || config.Roles["image-writer"] != policy.Roles["image-writer"] {
		t.Fatal("startup lost exact capability binding")
	}
	for _, alias := range []string{"Provider", "Service_account", "Service_account_name", "Templates"} {
		bad := strings.Replace(string(data), "\""+strings.ToLower(alias)+"\":", "\""+alias+"\":", 1)
		if bad == string(data) {
			t.Fatal("negative control did not alter a binding")
		}
		if err := os.WriteFile(name, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readArgoReleaseWIFConfig(name); err == nil {
			t.Fatalf("case-folded field %s accepted", alias)
		}
	}
}

func TestReadArgoReleaseWIFConfigFailsClosed(t *testing.T) {
	if c, err := readArgoReleaseWIFConfig(""); err != nil || c != nil {
		t.Fatal("empty option must disable federation")
	}
	if _, err := readArgoReleaseWIFConfig("relative.json"); err == nil {
		t.Fatal("relative capability file accepted")
	}
	for _, body := range []string{`{"unknown":true}`, `{} {}`, `not-json`, `null`, `{"roles":null}`, `{"Roles":{}}`, `{"roles":{},"roles":{}}`, `{"roles":{"image-writer":{"provider":"a","provider":"b"}}}`, `{"repositories":{"github/org/repo":{"templates":{"leaf":"image-reader","leaf":"image-writer"}}}}`, strings.Repeat(" ", (1<<20)+1)} {
		name := filepath.Join(t.TempDir(), "capabilities.json")
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readArgoReleaseWIFConfig(name); err == nil {
			t.Fatal("invalid capability file accepted")
		}
	}
	name := filepath.Join(t.TempDir(), "capabilities.json")
	if err := os.WriteFile(name, []byte(`{"roles":{},"repositories":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := readArgoReleaseWIFConfig(name)
	if err != nil || c == nil {
		t.Fatalf("empty deny-all capability file: %v", err)
	}
}
