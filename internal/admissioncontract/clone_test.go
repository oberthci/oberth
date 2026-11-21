package admissioncontract

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"testing"
)

func changeView(v *PreparedView) {
	v.Workflow.Spec.Templates[0].Name = "changed"
	v.SourceBindings[0].RepositoryID = 2
	v.SourceBindings[0].Export.Tags[0].ObjectSHA = strings.Repeat("d", 40)
	v.SourceReceipt.DocumentDigest = "changed"
	v.Defaults.WorkflowDefaults[0] = '['
	v.Templates[0].Spec[0] = '['
	v.Identity.Pipeline.UID = "changed"
	v.Identity.Resource.UID = "changed"
	v.Grants[0].Audience = "changed"
	v.Plan.Entries[0].Path = "changed"
	v.Findings[0].Reason = "changed"
}

func TestSealAndAccessorsShareNoMutableReferences(t *testing.T) {
	input := preparedFixture()
	input.Workflow.Labels = map[string]string{"bound": "yes"}
	sealed := mustSeal(t, input)
	before, _ := sealed.MarshalJSON()
	changeView(&input)
	input.Workflow.Labels["bound"] = "no"
	read := sealed.View()
	changeView(&read)
	read.Workflow.Labels["bound"] = "no"
	copy := *sealed
	copyRead := copy.View()
	changeView(&copyRead)
	after, err := sealed.MarshalJSON()
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("sealed state changed: %v", err)
	}
	copyJSON, err := copy.MarshalJSON()
	if err != nil || !bytes.Equal(before, copyJSON) {
		t.Fatal("clone changed sealed value")
	}
}

func TestInputClonesFreezeHooksAndRequests(t *testing.T) {
	view := preparedFixture()
	request := PrepareRequest{Document: []byte("authored"), Defaults: view.Defaults, Templates: view.Templates,
		SourceReceipt:   view.SourceReceipt,
		IdentityRequest: IdentityRequest{PipelineAccount: "pipeline"}, Grants: view.Grants}
	before, _ := json.Marshal(request)
	copy := request.Clone()
	copy.Document[0] = 'x'
	copy.Defaults.WorkflowDefaults[0] = '['
	copy.Templates[0].Spec[0] = '['
	copy.IdentityRequest.PipelineAccount = "changed"
	copy.Grants[0].Path = "changed"
	after, _ := json.Marshal(request)
	if !bytes.Equal(before, after) {
		t.Fatal("request clone changes input")
	}
	input := TransformInput{Identity: view.Identity, CredentialSteps: map[string]string{"build": "build"}, SecretGrants: map[string][]GrantEvidence{"build": view.Grants}, SourceBindings: view.SourceBindings, Defaults: view.Defaults}
	before, _ = json.Marshal(input)
	hookCopy := input.Clone()
	hookCopy.CredentialSteps["build"] = "changed"
	hookCopy.Identity.Pipeline.UID = "changed"
	hookCopy.Identity.Resource.UID = "changed"
	hookCopy.SecretGrants["build"][0].Path = "changed"
	hookCopy.SourceBindings[0].RepositoryID = 2
	hookCopy.SourceBindings[0].Export.Tags[0].ObjectSHA = strings.Repeat("d", 40)
	hookCopy.Defaults.WorkflowDefaults[0] = '['
	after, _ = json.Marshal(input)
	if !bytes.Equal(before, after) {
		t.Fatal("hook clone changes input")
	}
}

func TestClonesPreserveNilAndEmptyValues(t *testing.T) {
	for _, value := range []PreparedView{{}, {SourceBindings: []sourcecontract.Binding{}}, {SourceBindings: []sourcecontract.Binding{{Export: sourcecontract.Export{Tags: []sourcecontract.Ref{}}}}}, {Templates: []TemplateSnapshot{}, Grants: []GrantEvidence{}, Plan: Plan{Entries: []PlanEntry{}}}} {
		if !reflect.DeepEqual(value, value.Clone()) {
			t.Fatalf("clone changes %#v", value)
		}
	}
	if !reflect.DeepEqual(PrepareRequest{}, (PrepareRequest{}).Clone()) ||
		!reflect.DeepEqual(TransformInput{}, (TransformInput{}).Clone()) {
		t.Fatal("zero clone changed value")
	}
}

func TestSealedReadsAreConcurrentAndIndependent(t *testing.T) {
	sealed := mustSeal(t, preparedFixture())
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			for range 10 {
				view := sealed.View()
				changeView(&view)
				if _, err := sealed.MarshalJSON(); err != nil {
					t.Error(err)
				}
			}
		})
	}
	group.Wait()
}

func TestSpecDigestIgnoresMetadata(t *testing.T) {
	base := preparedFixture()
	first := mustSeal(t, base).View()
	base.Workflow.Labels = map[string]string{"audit": "bound"}
	second := mustSeal(t, base).View()
	if first.SpecDigest != second.SpecDigest || first.Digest == second.Digest {
		t.Fatal("metadata affects wrong digest")
	}
}
