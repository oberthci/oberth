package argojob

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/sourcecontract"
	core "k8s.io/api/core/v1"
	"math/big"
	"slices"
	"strings"
)

const (
	trustKey = "safe.directory"
	trustDir = "/work/src"
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
	return append(env, core.EnvVar{Name: "GIT_CONFIG_KEY_" + index, Value: trustKey}, core.EnvVar{Name: "GIT_CONFIG_VALUE_" + index, Value: trustDir})
}

// hasSourceTrust reports whether an authored entry Git reads already trusts
// the source directory: an index below the count, the key without letter case,
// exactly the value Oberth would add, and no later empty entry, which resets
// Git's list. Only the exact value counts: a step runs its image's own Git,
// and an older Git compares the configured path literally, so another spelling
// of the directory keeps Oberth's exact entry beside it. The authored entry
// stays exactly as written.
func hasSourceTrust(values map[string]string, count *big.Int) bool {
	var trust, reset *big.Int
	for key, value := range values {
		index, ok := strings.CutPrefix(key, "GIT_CONFIG_KEY_")
		if !ok || !gitKeyIs(value, trustKey) {
			continue
		}
		number, ok := readIndex(index, count)
		if !ok {
			continue
		}
		switch values["GIT_CONFIG_VALUE_"+index] {
		case "":
			reset = later(reset, number)
		case trustDir:
			trust = later(trust, number)
		}
	}
	return trust != nil && (reset == nil || trust.Cmp(reset) > 0)
}

// readIndex accepts an entry index Git reads: written in plain decimal, as Git
// names the variables, and below the count.
func readIndex(index string, count *big.Int) (*big.Int, bool) {
	number, ok := new(big.Int).SetString(index, 10)
	return number, ok && number.String() == index && number.Sign() >= 0 && number.Cmp(count) < 0
}

// gitKeyIs compares configuration keys as Git does, folding ASCII letters only:
// equal byte lengths keep Unicode foldings such as "ſ" for "s" out.
func gitKeyIs(key, want string) bool {
	return len(key) == len(want) && strings.EqualFold(key, want)
}

func later(current, index *big.Int) *big.Int {
	if current == nil || index.Cmp(current) > 0 {
		return index
	}
	return current
}
