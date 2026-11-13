package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type absentBranchPolicy struct{ err error }

func (p absentBranchPolicy) ReadOptionalBlob(context.Context, string, string, string, int) ([]byte, bool, error) {
	return nil, false, p.err
}
func (p absentBranchPolicy) ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error) {
	return strings.Repeat("b", 40), nil, false, p.err
}

func branchPlacementFixture(t *testing.T) (Config, Request) {
	t.Helper()
	c, r, _ := placementFixture(t)
	r.SourceVolume = SourceVolume{}
	d := c.ExecutionPlacement.document
	d.Schema = branchPlacementSchema
	d.Normal = placementRun{RunID: r.RunID, Workflow: r.Name}
	proof, err := workflowpolicy.ResolveBranchAuthority(t.Context(), absentBranchPolicy{}, d.Repository, r.SHA)
	if err != nil {
		t.Fatal(err)
	}
	r.BranchAuthority = proof
	d.BranchPolicyBaseline, d.PolicyDigest = proof.Identity()
	resolved, err := argoworkflow.ResolveRunSource(r.Source, r.Fragments, argoworkflow.RunSourceBinding{RunID: r.RunID, RepositoryID: r.RepositoryID, SHA: r.SHA, File: r.PipelineFile, Trigger: r.effectiveRunTrigger()})
	if err != nil {
		t.Fatal(err)
	}
	d.GraphDigest = resolved.GraphIdentity()
	d.FragmentLockDigest = placementHash(resolved.Lock())
	p := &ExecutionPlacement{document: d}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	c.ExecutionPlacement, err = ParseExecutionPlacement(body)
	if err != nil {
		t.Fatal(err)
	}
	return c, r
}

func TestBranchPlacementClosedNativeAuthorityAndLegacySeparation(t *testing.T) {
	c, r := branchPlacementFixture(t)
	if _, err := Build(c, r); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Request){"missing native proof": func(r *Request) { r.BranchAuthority = nil }, "different run": func(r *Request) { r.RunID += "other" }, "different source": func(r *Request) { r.Source = append(append([]byte(nil), r.Source...), '\n') }, "foreign repo": func(r *Request) { r.UpstreamOrg = "foreign" }} {
		t.Run(name, func(t *testing.T) {
			copy := r
			change(&copy)
			if _, err := Build(c, copy); err == nil {
				t.Fatal("closed branch selection bypassed")
			}
		})
	}
	body, err := json.Marshal(c.ExecutionPlacement)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		t.Fatal("bad policy")
	}
	if _, ok := fields["preflight"]; ok {
		t.Fatal("dummy preflight serialized")
	}
	fields["preflight"] = json.RawMessage(`null`)
	body, _ = json.Marshal(fields)
	if _, err := ParseExecutionPlacement(body); err == nil {
		t.Fatal("explicit dummy preflight accepted")
	}
	fields["preflight"] = json.RawMessage(`{}`)
	body, _ = json.Marshal(fields)
	if _, err := ParseExecutionPlacement(body); err == nil {
		t.Fatal("empty dummy preflight accepted")
	}
	if proof, err := workflowpolicy.ResolveBranchAuthority(t.Context(), absentBranchPolicy{err: errors.New("denied")}, "codeberg/acme/oberth", r.SHA); err == nil || proof != nil {
		t.Fatal("failed native policy read became absence")
	}
}

func TestBranchPlacementAuthenticatesWholeFragmentInventory(t *testing.T) {
	c, r := branchPlacementFixture(t)
	r.Source = []byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: ci
  templates:
  - name: ci
    dag:
      tasks:
      - name: call
        templateRef: {name: codeberg/acme/shared@v1, template: leaf}
`)
	key := argoworkflow.FragmentKey{Repo: "codeberg/acme/shared", Version: "v1"}
	fragment := argoworkflow.Fragment{Key: key, SHA: strings.Repeat("d", 40), Source: []byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: leaf
  templates:
  - name: leaf
    container:
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
`)}
	r.Fragments = map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: fragment}
	resolved, err := argoworkflow.ResolveRunSource(r.Source, r.Fragments, argoworkflow.RunSourceBinding{RunID: r.RunID, RepositoryID: r.RepositoryID, SHA: r.SHA, File: r.PipelineFile, Trigger: r.effectiveRunTrigger()})
	if err != nil {
		t.Fatal(err)
	}
	d := c.ExecutionPlacement.document
	d.SourceDigest = placementHashRaw(r.Source)
	d.GraphDigest = resolved.GraphIdentity()
	d.FragmentLockDigest = placementHash(resolved.Lock())
	d.Fragments = []placementFragment{{Key: key.String(), Commit: fragment.SHA, Digest: placementHashRaw(fragment.Source)}}
	p := &ExecutionPlacement{document: d}
	body, _ := json.Marshal(p)
	c.ExecutionPlacement, err = ParseExecutionPlacement(body)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Build(c, r); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"missing", "extra", "commit", "key", "raw bytes with copied digest", "declared digest", "changed graph"} {
		t.Run(mode, func(t *testing.T) {
			copy := r
			copy.Fragments = map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: fragment}
			changed := fragment
			switch mode {
			case "missing":
				delete(copy.Fragments, key)
			case "extra":
				extra := key
				extra.Version = "v2"
				copy.Fragments[extra] = fragment
			case "commit":
				changed.SHA = strings.Repeat("e", 40)
				copy.Fragments[key] = changed
			case "key":
				changed.Key.Version = "v2"
				copy.Fragments[key] = changed
			case "raw bytes with copied digest":
				changed.Source = append(append([]byte(nil), fragment.Source...), '\n')
				changed.Digest = d.Fragments[0].Digest
				copy.Fragments[key] = changed
			case "declared digest":
				changed.Digest = strings.Repeat("f", 64)
				copy.Fragments[key] = changed
			case "changed graph":
				copy.Source = append(append([]byte(nil), r.Source...), '\n')
			}
			if err := c.ExecutionPlacement.selectRequest(c, copy); err == nil {
				t.Fatal("fragment/source rebinding accepted")
			}
		})
	}
	var fields map[string]json.RawMessage
	body, _ = json.Marshal(c.ExecutionPlacement)
	_ = json.Unmarshal(body, &fields)
	fields["normal"] = json.RawMessage(`{"run_id":"x","workflow":"y","Trusted_Intent":null}`)
	body, _ = json.Marshal(fields)
	if _, err := ParseExecutionPlacement(body); err == nil {
		t.Fatal("trusted intent placeholder accepted")
	}
}
