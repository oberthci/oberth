package argojob

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/protectedtests"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const protectedFragmentRoot = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: ci
  templates:
  - name: ci
    dag:
      tasks:
      - name: producer
        templateRef: {name: example/shared@v1, template: build}
`
const protectedFragmentBody = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  templates:
  - name: build
    container:
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [sh, -c]
      args: ["echo ok"]
      resources:
        requests: {cpu: 100m, memory: 64Mi}
        limits: {cpu: 1, memory: 64Mi}
`

func protectedFragmentRequest(t *testing.T, source []byte, fragments map[argoworkflow.FragmentKey]argoworkflow.Fragment) (Config, Request) {
	t.Helper()
	config, r := protectedPlacementFixture(t)
	r.Source, r.Fragments = source, fragments
	resolved, err := argoworkflow.ResolveRunSource(source, fragments, argoworkflow.RunSourceBinding{RunID: r.RunID, RepositoryID: r.RepositoryID, SHA: r.SHA, File: r.PipelineFile, Trigger: argoworkflow.RunTriggerBranch})
	if err != nil {
		t.Fatal(err)
	}
	peak, err := argoworkflow.DeclaredCapabilityPeak(resolved.Workflow())
	if err != nil {
		t.Fatal(err)
	}
	if peak != nil && peak.ExtendedResources[protectedtests.GenericDeviceResource] > 1 {
		r.Capability.ExtendedResources[protectedtests.GenericDeviceResource] = peak.ExtendedResources[protectedtests.GenericDeviceResource]
	}
	intent := *r.TrustedKVM
	intent.WorkflowSpecDigest = placementHashRaw(source)
	admission, err := protectedtests.NewGenericAdmission(intent, r.Capability, config.TrustedKVMGroup)
	if err != nil {
		t.Fatal(err)
	}
	r.GenericProtected, r.TrustedKVM = &admission, &admission.Intent
	d := config.ExecutionPlacement.document
	policy := *d.ProtectedPolicy
	policy.CapabilityDigest = protectedtests.CapabilityDigest(*r.Capability)
	d.ProtectedPolicy = &policy
	d.SourceDigest = intent.WorkflowSpecDigest
	d.GraphDigest, d.FragmentLockDigest = resolved.GraphIdentity(), placementHash(resolved.Lock())
	for _, fragment := range resolved.Lock() {
		d.Fragments = append(d.Fragments, placementFragment{Key: fragment.Key.String(), Commit: fragment.SHA, Digest: fragment.Digest})
	}
	config.ExecutionPlacement = &ExecutionPlacement{document: d, digest: placementHash(d)}
	raw, err := json.Marshal(config.ExecutionPlacement)
	if err != nil {
		t.Fatal(err)
	}
	config.ExecutionPlacement, err = ParseExecutionPlacement(raw)
	if err != nil {
		t.Fatal(err)
	}
	return config, prepareTrustedTestRequest(t, config, r)
}

func TestProtectedFragmentGraphAndRecoveryCapsule(t *testing.T) {
	key, err := argoworkflow.ParseFragmentRef("example/shared@v1")
	if err != nil {
		t.Fatal(err)
	}
	fragments := map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: {Key: key, SHA: strings.Repeat("a", 40), Source: []byte(protectedFragmentBody)}}
	config, r := protectedFragmentRequest(t, []byte(protectedFragmentRoot), fragments)
	if _, err = Build(config, r); err != nil {
		t.Fatal(err)
	}
	a, err := ProtectedAdmission(config, r)
	if err != nil || a == nil || a.Version != 2 {
		t.Fatal("fragment source admission missing", err)
	}
	source, retained, err := a.RestoreSources(*r.GenericProtected)
	if err != nil || !bytes.Equal(source, r.Source) || !bytes.Equal(retained[key].Source, fragments[key].Source) {
		t.Fatal("original bytes lost", err)
	}
	source[0] = 'x'
	retained[key].Source[0] = 'x'
	if err = a.Validate(*r.GenericProtected); err != nil {
		t.Fatal("returned source aliases audit custody", err)
	}
	for _, mode := range []string{"missing", "changed bytes", "changed commit", "extra"} {
		t.Run(mode, func(t *testing.T) {
			altered := r
			altered.Fragments = make(map[argoworkflow.FragmentKey]argoworkflow.Fragment)
			for k, f := range r.Fragments {
				altered.Fragments[k] = f
			}
			f := altered.Fragments[key]
			switch mode {
			case "missing":
				delete(altered.Fragments, key)
			case "changed bytes":
				f.Source = append([]byte("# changed\n"), f.Source...)
				altered.Fragments[key] = f
			case "changed commit":
				f.SHA = strings.Repeat("b", 40)
				altered.Fragments[key] = f
			case "extra":
				k := argoworkflow.FragmentKey{Repo: "example/other", Version: "v1"}
				f.Key = k
				altered.Fragments[k] = f
			}
			if _, e := Build(config, altered); e == nil {
				t.Fatal("changed fragment graph accepted")
			}
		})
	}
	// Both phases retain the same original authored graph and lock.
	normal := r
	normal.TrustedKVM = nil
	normal.GenericProtected = nil
	normal.Name = config.ExecutionPlacement.document.NormalWorkflow
	normal.SourceVolume = SourceVolume{}
	normal = prepareTrustedTestRequest(t, config, normal)
	if _, err = Build(config, normal); err != nil {
		t.Fatal("parent rejected original fragment graph", err)
	}
	for _, mode := range []string{"old version with capsule", "new version without capsule", "wrong input digest", "oversized source"} {
		t.Run(mode, func(t *testing.T) {
			raw, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			var altered protectedtests.PlacementAdmission
			if err = json.Unmarshal(raw, &altered); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "old version with capsule":
				altered.Version = 1
			case "new version without capsule":
				altered.Sources = nil
			case "wrong input digest":
				altered.Sources.Fragments[0].InputDigest = strings.Repeat("f", 64)
			case "oversized source":
				altered.Sources.Source = make([]byte, 512<<10)
			}
			if altered.Validate(*r.GenericProtected) == nil {
				t.Fatal("invalid reconstruction capsule accepted")
			}
		})
	}
	a.Sources.Fragments[0].Source[0] = 'x'
	if a.Validate(*r.GenericProtected) == nil {
		t.Fatal("tampered custody accepted")
	}
}

func TestProtectedFragmentAuthenticExternalGraph(t *testing.T) {
	directory := os.Getenv("OBERTH_PROTECTED_FRAGMENT_FIXTURE")
	if directory == "" {
		t.Skip("set OBERTH_PROTECTED_FRAGMENT_FIXTURE to the sealed public source fixture")
	}
	read := func(name string) []byte {
		body, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	var manifest struct {
		Key            string            `json:"key"`
		ProducerCommit string            `json:"producer_commit"`
		Files          map[string]string `json:"files"`
	}
	if err := json.Unmarshal(read("manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	source, fragment := read("build.yaml"), read("fragment.yaml")
	// Independent published input pins; never derive expected bytes from Build.
	if placementHashRaw(source) != "6e5a4d596d5482655904674d021f8886a8f5db7025cf6703d1e6502493f02d15" || placementHashRaw(fragment) != "6bde67e6e5f1da9a46aa5dc8350f8079e451d30ee9f90c0d7549826abd5e2247" || manifest.ProducerCommit != "904667f7b00ff834af10788a4f8e47523293df11" {
		t.Fatal("authentic fixture differs from published input pins")
	}
	if strings.Count(string(source), "templateRef:") != 4 {
		t.Fatal("authentic fragment calls removed")
	}
	key, err := argoworkflow.ParseFragmentRef(manifest.Key)
	if err != nil {
		t.Fatal(err)
	}
	fragments := map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: {Key: key, SHA: manifest.ProducerCommit, Source: fragment}}
	oldConfig, oldRequest := protectedFragmentRequest(t, source, fragments)
	if _, err = Build(oldConfig, oldRequest); err == nil {
		t.Fatal("legacy builtin annotation acquired generic authority")
	}
	source = read("build-generic.yaml")
	if placementHashRaw(source) != "c75a29404ae9024121b0d4892835b1ea7762972e549367b61a2a6534a0beda25" || strings.Count(string(source), "templateRef:") != 4 {
		t.Fatal("prepared root or authentic calls changed")
	}
	config, r := protectedFragmentRequest(t, source, fragments)
	if r.Capability.ExtendedResources[protectedtests.GenericDeviceResource] != 3 {
		t.Fatal("authentic current graph device peak differs from reviewed example")
	}
	wf, err := Build(config, r)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.Entrypoint != trustedKVMWrapper {
		t.Fatal("protected host wrapping lost")
	}
	a, err := ProtectedAdmission(config, r)
	if err != nil {
		t.Fatal(err)
	}
	restored, locked, err := a.RestoreSources(*r.GenericProtected)
	if err != nil || !bytes.Equal(restored, source) || !bytes.Equal(locked[key].Source, fragment) {
		t.Fatal("authentic source custody changed", err)
	}
	normal := r
	normal.TrustedKVM = nil
	normal.GenericProtected = nil
	normal.Name = config.ExecutionPlacement.document.NormalWorkflow
	normal.SourceVolume = SourceVolume{}
	normal = prepareTrustedTestRequest(t, config, normal)
	if _, err = Build(config, normal); err != nil {
		t.Fatal(err)
	}
	t.Logf("original source=%s fragment=%s graph=%s lock=%s preflight_templates=%d", placementHashRaw(source), placementHashRaw(fragment), config.ExecutionPlacement.document.GraphDigest, config.ExecutionPlacement.document.FragmentLockDigest, len(wf.Spec.Templates))
}
