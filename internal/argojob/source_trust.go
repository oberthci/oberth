package argojob

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/sourcecontract"
	core "k8s.io/api/core/v1"
	"math/big"
	"slices"
	"strings"
)

func appendSourceTrust(t *wf.Template, bindings []sourcecontract.Binding) {
	if !slices.ContainsFunc(bindings, func(b sourcecontract.Binding) bool {
		return b.Template == t.Name && b.Descriptor.Kind == sourcecontract.Main
	}) {
		return
	}
	if t.Container != nil {
		t.Container.Env = sourceTrust(t.Container.Env)
	}
	if t.Script != nil {
		t.Script.Env = sourceTrust(t.Script.Env)
	}
	if t.ContainerSet != nil {
		for i := range t.ContainerSet.Containers {
			t.ContainerSet.Containers[i].Env = sourceTrust(t.ContainerSet.Containers[i].Env)
		}
	}
}

func sourceTrust(env []core.EnvVar) []core.EnvVar {
	values := map[string]string{}
	countAt := -1
	count := new(big.Int)
	for i, v := range env {
		values[v.Name] = v.Value
		if v.Name == "GIT_CONFIG_COUNT" {
			countAt = i
		}
	}
	if _, ok := count.SetString(values["GIT_CONFIG_COUNT"], 10); !ok || count.Sign() < 0 {
		count.SetInt64(0)
	}
	if hasSourceTrust(values, count) {
		return env
	}
	index := count.String()
	next := core.EnvVar{Name: "GIT_CONFIG_COUNT", Value: count.Add(count, big.NewInt(1)).String()}
	if countAt < 0 {
		env = append(env, next)
	} else {
		env[countAt] = next
	}
	return append(env, core.EnvVar{Name: "GIT_CONFIG_KEY_" + index, Value: "safe.directory"}, core.EnvVar{Name: "GIT_CONFIG_VALUE_" + index, Value: "/work/src"})
}

func hasSourceTrust(values map[string]string, count *big.Int) bool {
	for key, value := range values {
		index, ok := strings.CutPrefix(key, "GIT_CONFIG_KEY_")
		if !ok || value != "safe.directory" {
			continue
		}
		number, ok := new(big.Int).SetString(index, 10)
		if ok && number.Sign() >= 0 && number.Cmp(count) < 0 && values["GIT_CONFIG_VALUE_"+index] == "/work/src" {
			return true
		}
	}
	return false
}
