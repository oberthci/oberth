package argojob

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/protectedtests"
)

func protectedPlacementFixture(t *testing.T) (Config, Request) {
	t.Helper()
	base, _, _ := placementFixture(t)
	config, r := genericProtectedRequest(t)
	config.KVMEnabled = true
	config.SourceStorageClass = base.SourceStorageClass
	config.GoProxyURL = base.GoProxyURL
	config.GoProxyModulePrefix = base.GoProxyModulePrefix
	config.GoProxyCACertPEM = base.GoProxyCACertPEM
	config.applyDefaults()
	r.Name = "native-parent-trusted"
	r.PipelineFile = ".oberth/build.yaml"
	intent := *r.TrustedKVM
	intent.WorkflowName = r.Name
	a, err := protectedtests.NewGenericAdmission(intent, r.Capability, config.TrustedKVMGroup)
	if err != nil {
		t.Fatal(err)
	}
	r.TrustedKVM = &a.Intent
	r.GenericProtected = &a
	d := base.ExecutionPlacement.document
	d.Schema = protectedPlacementSchema
	d.Normal = placementRun{}
	d.Preflight = placementRun{}
	d.ParentRunID = r.RunID
	d.NormalWorkflow = "native-parent"
	d.PreflightWorkflow = r.Name
	d.SourceDigest = placementHashRaw(r.Source)
	d.ServiceAccount = config.PipelineServiceAccount
	d.DependencyIdentity = placementDependencyIdentity(config)
	d.ProtectedPolicy = &protectedtests.PlacementPolicy{BaselineSHA: a.Intent.BaselineSHA, Contract: a.Intent.Contract, Recipe: a.Intent.Recipe, InventoryDigest: a.Intent.InventoryDigest, CaseCount: a.Intent.CaseCount, CapabilityDigest: protectedtests.CapabilityDigest(a.Capability), SupplementalGroup: a.SupplementalGroup, MaxPhaseSeconds: 3600}
	p := &ExecutionPlacement{document: d, digest: placementHash(d)}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	config.ExecutionPlacement, err = ParseExecutionPlacement(body)
	if err != nil {
		t.Fatal(err)
	}
	r.SourceVolume = SourceVolume{}
	return config, prepareTrustedTestRequest(t, config, r)
}

func TestProtectedPlacementUsesOneParentAndDynamicIntent(t *testing.T) {
	config, r := protectedPlacementFixture(t)
	if _, err := Build(config, r); err != nil {
		t.Fatal(err)
	}
	admission, err := ProtectedAdmission(config, r)
	if err != nil {
		t.Fatal(err)
	}
	if admission == nil || admission.Validate(*r.GenericProtected) != nil {
		t.Fatal("missing exact placement custody")
	}
	normal := r
	normal.TrustedKVM = nil
	normal.GenericProtected = nil
	normal.Name = config.ExecutionPlacement.document.NormalWorkflow
	normal.SourceVolume = SourceVolume{}
	if _, err = Build(config, normal); err != nil {
		t.Fatal(err)
	}
	// Old exact-policy constraints cannot be silently reinterpreted as new ones.
	d := config.ExecutionPlacement.document
	d.Schema = "oberth.execution-placement/v1"
	p := &ExecutionPlacement{document: d, digest: placementHash(d)}
	if p.validate() == nil {
		t.Fatal("new contract accepted as old v1")
	}
	if _, err = Build(config, Request{}); err == nil {
		t.Fatal("unbound request accepted")
	}
	missingFile := r
	missingFile.PipelineFile = ""
	if _, err = Build(config, missingFile); err == nil {
		t.Fatal("missing actual pipeline file accepted")
	}
}

func TestProtectedPlacementRejectsIndependentAuthorityChanges(t *testing.T) {
	for name, change := range map[string]func(*Config, *Request){
		"parent":            func(_ *Config, r *Request) { r.RunID = "another" },
		"workflow":          func(_ *Config, r *Request) { r.Name = "another" },
		"source":            func(_ *Config, r *Request) { r.Source = append(r.Source, '\n') },
		"repo":              func(_ *Config, r *Request) { r.RepositoryID++ },
		"ref":               func(_ *Config, r *Request) { r.Ref = "another" },
		"generic absent":    func(_ *Config, r *Request) { r.GenericProtected = nil },
		"capability absent": func(_ *Config, r *Request) { r.Capability = nil },
		"group":             func(c *Config, _ *Request) { c.TrustedKVMGroup++ },
		"baseline":          func(_ *Config, r *Request) { r.GenericProtected.Intent.BaselineSHA = strings.Repeat("0", 40) },
		"inventory":         func(_ *Config, r *Request) { r.GenericProtected.Intent.InventoryDigest = strings.Repeat("0", 64) },
		"case count":        func(_ *Config, r *Request) { r.GenericProtected.Intent.CaseCount++ },
		"artifact":          func(_ *Config, r *Request) { r.GenericProtected.Intent.Contract.Artifacts = []string{"other"} },
		"harness": func(_ *Config, r *Request) {
			r.GenericProtected.Intent.Recipe.HarnessImage = "example.org/other@sha256:" + strings.Repeat("0", 64)
		},
		"expired": func(c *Config, _ *Request) {
			d := c.ExecutionPlacement.document
			d.Expires = time.Now().Add(-time.Second)
			c.ExecutionPlacement = &ExecutionPlacement{document: d, digest: placementHash(d)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			config, r := protectedPlacementFixture(t)
			change(&config, &r)
			if _, err := Build(config, r); err == nil {
				t.Fatal("changed authority admitted")
			}
		})
	}
}

func TestProtectedPlacementStartupExpiryNeverRenewsAdmission(t *testing.T) {
	config, r := protectedPlacementFixture(t)
	d := config.ExecutionPlacement.document
	d.Expires = time.Now().Add(-time.Second)
	p := &ExecutionPlacement{document: d, digest: placementHash(d)}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ParseExecutionPlacement(body); err == nil {
		t.Fatal("expired admission accepted")
	}
	restored, err := ParseExecutionPlacementForStartup(body)
	if err != nil {
		t.Fatal(err)
	}
	config.ExecutionPlacement = restored
	if _, err = Build(config, r); err == nil {
		t.Fatal("startup renewed expired placement")
	}
}
