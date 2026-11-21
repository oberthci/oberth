package argoworkflow

import (
	"reflect"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"k8s.io/apimachinery/pkg/util/validation"
)

func validateIdentity(request admissioncontract.IdentityRequest, e admissioncontract.IdentityEvidence, submit bool) error {
	valid := identityEnvelope(request, e, submit)
	seen := map[string]bool{e.Pipeline.Name: true}
	if request.ResourceAccount != "" {
		if e.Resource == nil || e.Resource.Name != request.ResourceAccount || !accountValid(*e.Resource, seen, submit) {
			valid = false
		}
	} else if e.Resource != nil {
		valid = false
	}
	if !valid {
		return refusal("identity", "identity")
	}
	return nil
}
func accountValid(a admissioncontract.AccountEvidence, seen map[string]bool, submit bool) bool {
	if seen[a.Name] || a.Name == "default" || len(validation.IsDNS1123Subdomain(a.Name)) > 0 || submit && a.UID == "" {
		return false
	}
	seen[a.Name] = true
	return true
}

// bindIdentity replaces authored identities with the assigned execution accounts.
func bindIdentity(wf *wfv1.Workflow, e admissioncontract.IdentityEvidence) {
	wf.Status = wfv1.WorkflowStatus{}
	wf.Namespace = e.Namespace
	if wf.Spec.Executor == nil {
		wf.Spec.Executor = &wfv1.ExecutorConfig{}
	}
	binding := identityBinding{IdentityEvidence: e}
	binding.normalizeIdentity(reflect.ValueOf(wf).Elem())
}

type identityBinding struct {
	admissioncontract.IdentityEvidence
}

func (b *identityBinding) normalizeIdentity(value reflect.Value) {
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !value.IsNil() {
			b.normalizeIdentity(value.Elem())
		}
	case reflect.Struct:
		b.normalizeStruct(value)
	case reflect.Slice, reflect.Array:
		for i := range value.Len() {
			b.normalizeIdentity(value.Index(i))
		}
	case reflect.Map:
		b.normalizeMap(value)
	}
}
func (b *identityBinding) normalizeStruct(value reflect.Value) {
	for i := range value.NumField() {
		if !value.Type().Field(i).IsExported() {
			continue
		}
		field := value.Field(i)
		switch value.Type().Field(i).Name {
		case "ServiceAccountName":
			field.SetString(b.Pipeline.Name)
		case "AutomountServiceAccountToken":
			disabled := false
			field.Set(reflect.ValueOf(&disabled))
		default:
			b.normalizeIdentity(field)
		}
	}
	if value.Type() == reflect.TypeFor[wfv1.Template]() && b.Resource != nil && !value.FieldByName("Resource").IsNil() {
		value.FieldByName("ServiceAccountName").SetString(b.Resource.Name)
		value.FieldByName("Executor").Set(reflect.ValueOf(&wfv1.ExecutorConfig{ServiceAccountName: b.Pipeline.Name}))
	}
}
func (b *identityBinding) normalizeMap(value reflect.Value) {
	if value.Type().Elem().Kind() == reflect.String {
		return
	}
	for _, key := range value.MapKeys() {
		copy := reflect.New(value.Type().Elem()).Elem()
		copy.Set(value.MapIndex(key))
		b.normalizeIdentity(copy)
		value.SetMapIndex(key, copy)
	}
}
func (g *graph) checkResourceIdentity(e admissioncontract.IdentityEvidence) error {
	if g.offline() && e.State != admissioncontract.StateVerified {
		return nil
	}
	for _, n := range g.nodes {
		if n.template.Resource == nil {
			continue
		}
		c := g.policy.Capability
		if c == nil || e.Resource == nil || c.ServiceAccount != e.Resource.Name {
			return refusal(n.path+".resource", "identity")
		}
	}
	return nil
}
func scheduleValid(trigger, entry string) bool {
	return len(entry) <= admissioncontract.MaxScheduleEntryBytes && !strings.ContainsAny(entry, "{}\x00\r\n") && ((trigger == "schedule") == (entry != ""))
}

func identityEnvelope(r admissioncontract.IdentityRequest, e admissioncontract.IdentityEvidence, submit bool) bool {
	if len(validation.IsDNS1123Label(e.Namespace)) != 0 || r.Namespace != e.Namespace || !e.State.Valid() {
		return false
	}
	if r.PipelineAccount != e.Pipeline.Name || len(validation.IsDNS1123Subdomain(e.Pipeline.Name)) != 0 || e.Pipeline.Name == "default" {
		return false
	}
	if submit && (e.State != admissioncontract.StateVerified || e.Pipeline.UID == "") {
		return false
	}
	return true
}
