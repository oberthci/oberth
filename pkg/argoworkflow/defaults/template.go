package defaults

import (
	"encoding/json"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"reflect"
)

// Template returns a detached effective template using native strategic merge.
func Template(base, authored *wfv1.Template) (*wfv1.Template, error) {
	if base == nil {
		return authored.DeepCopy(), nil
	}
	kind := authored.GetType()
	applicable := base.DeepCopy()
	value := reflect.ValueOf(applicable).Elem()
	for i := range value.NumField() {
		field := value.Type().Field(i)
		if t := wfv1.TemplateType(field.Name); field.Type.Kind() == reflect.Pointer && field.Type.Elem().Kind() == reflect.Struct && t.IsValid() && t != kind {
			value.Field(i).SetZero()
		}
	}
	before, err := json.Marshal(applicable)
	if err != nil {
		return nil, err
	}
	after, err := json.Marshal(authored)
	if err != nil {
		return nil, err
	}
	merged, err := strategicpatch.StrategicMergePatch(before, after, wfv1.Template{})
	if err != nil {
		return nil, err
	}
	var result wfv1.Template
	if err := json.Unmarshal(merged, &result); err != nil {
		return nil, err
	}
	result.SetType(kind)
	return &result, nil
}
