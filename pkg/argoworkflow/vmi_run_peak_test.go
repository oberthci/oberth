package argoworkflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argologging "github.com/argoproj/argo-workflows/v4/util/logging"
	argotemplate "github.com/argoproj/argo-workflows/v4/util/template"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func runVMIFixture(t testing.TB) ([]byte, map[FragmentKey]Fragment, RunSourceBinding, Policy) {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	fragments := map[FragmentKey]Fragment{}
	for _, name := range []string{"toolchain", "driver"} {
		key := FragmentKey{Repo: "example/" + name, Version: "v1"}
		body := read("vmi-run-" + name + "-fragment.yaml")
		hash := sha256.Sum256(body)
		fragments[key] = Fragment{Key: key, SHA: strings.Repeat("a", 40), Digest: hex.EncodeToString(hash[:]), Source: body}
	}
	binding := RunSourceBinding{RunID: "run", RepositoryID: 7, SHA: strings.Repeat("b", 40), File: BuildFile, Trigger: RunTriggerBranch}
	policy := Policy{KubeVirt: true, ContainerDiskPrefixes: []string{"quay.io/example/"}, RunnerImagePrefixes: []string{"busybox:"}, VMIBudget: VMIBudgetLimit()}
	return read("vmi-run-projection.yaml"), fragments, binding, policy
}

func TestRunVMILifetimeProjection(t *testing.T) {
	source, fragments, binding, policy := runVMIFixture(t)
	for _, trigger := range []RunTrigger{RunTriggerBranch, RunTriggerPromotion, RunTriggerSchedule} {
		t.Run(string(trigger), func(t *testing.T) {
			binding.Trigger = trigger
			r, err := ResolveRunSource(source, fragments, binding)
			if err != nil {
				t.Fatal(err)
			}
			before := r.Workflow()
			contextFree, err := DeclaredVMIDemand(before)
			if err != nil || contextFree.VMs != 4 {
				t.Fatal(contextFree, err)
			}
			receipt, err := r.Receipt(policy)
			want := VMIDemand{VMs: 2, Cores: 4, MemoryBytes: 12 << 30}
			if err != nil || receipt.Summary().Demand != want || !receipt.Summary().Valid() {
				t.Fatalf("unexpected receipt or error: %v", err)
			}
			if !reflect.DeepEqual(before, r.Workflow()) {
				t.Fatal("sizing rewrote the failure-cleanup DAG or onExit")
			}
		})
	}
}

func mutateRunVMI(t *testing.T, source []byte, change func(*wfv1.Workflow)) []byte {
	t.Helper()
	wf, err := Decode(source)
	if err != nil {
		t.Fatal(err)
	}
	change(wf)
	b, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func runVMITemplate(wf *wfv1.Workflow, name string) *wfv1.Template {
	for i := range wf.Spec.Templates {
		if wf.Spec.Templates[i].Name == name {
			return &wf.Spec.Templates[i]
		}
	}
	panic(name)
}

func TestRunVMIPartitionProof(t *testing.T) {
	source, fragments, binding, policy := runVMIFixture(t)
	binding.Trigger = RunTriggerSchedule
	for _, value := range []string{"0", "1", "2", "3", "4", "5", "-1", "unknown", "", "{{workflow.parameters.unbound}}"} {
		t.Run("explicit-input/"+value, func(t *testing.T) {
			modified := mutateRunVMI(t, source, func(wf *wfv1.Workflow) {
				runVMITemplate(wf, "main").DAG.Tasks[3].Arguments.Parameters[0].Value = wfv1.AnyStringPtr(value)
			})
			r, err := ResolveRunSource(modified, fragments, binding)
			if err != nil {
				t.Fatal(err)
			}
			demand, err := r.demand()
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if value == "5" || value == "-1" || value == "unknown" {
				want = 0
			}
			if value == "" || strings.Contains(value, "{{") {
				want = 4
			}
			if demand.VMs != want {
				t.Fatalf("value=%q demand=%+v want=%d", value, demand, want)
			}
			_, err = r.Receipt(policy)
			if (want > policy.VMIBudget.MaxVMs) != (err != nil) {
				t.Fatalf("uncertain input must not bypass the grant: %v", err)
			}
		})
	}
	for _, kind := range []string{"unconditional", "duplicate-equality", "or-output-expression", "missing-input", "yaml-input-default", "producer-not-ancestor", "different-producer"} {
		t.Run(kind, func(t *testing.T) {
			modified := mutateRunVMI(t, source, func(wf *wfv1.Workflow) {
				parts := runVMITemplate(wf, "partitions")
				main := runVMITemplate(wf, "main")
				switch kind {
				case "unconditional":
					for i := range parts.DAG.Tasks {
						parts.DAG.Tasks[i].When = ""
					}
				case "duplicate-equality":
					parts.DAG.Tasks[1].When = parts.DAG.Tasks[0].When
				case "or-output-expression":
					parts.DAG.Tasks[0].When = "{{=inputs.parameters['partition'] == '0' || inputs.parameters['partition'] == '1'}}"
				case "missing-input":
					main.DAG.Tasks[3].Arguments.Parameters = nil
				case "yaml-input-default":
					main.DAG.Tasks[3].Arguments.Parameters = nil
					parts.Inputs.Parameters[0].Default = wfv1.AnyStringPtr("0")
				case "producer-not-ancestor":
					main.DAG.Tasks[3].Depends = "install.Succeeded"
				case "different-producer":
					parts.DAG.Tasks[0].When = "{{=tasks['some-other'].outputs.parameters['partition'] == '0'}}"
				}
			})
			r, err := ResolveRunSource(modified, fragments, binding)
			if err != nil {
				t.Fatal(err)
			}
			demand, err := r.demand()
			if err != nil {
				t.Fatal(err)
			}
			if demand.VMs < 4 {
				t.Fatalf("missing exclusivity proof earned two-VM credit: %+v", demand)
			}
			if kind == "unconditional" && demand.VMs != 10 {
				t.Fatalf("unconditional partitions must need 10 VMs: %+v", demand)
			}
		})
	}
}

func TestRunVMIProjectionFailsClosed(t *testing.T) {
	source, fragments, binding, _ := runVMIFixture(t)
	for _, kind := range []string{"create-retry", "delete-retry", "unbounded-get-retry", "fanout-items", "fanout-param", "fanout-sequence", "cycle", "recursive", "oversize", "expanded-lifetime-oversize", "unsupported-status", "continue-on", "exit-create", "omitted-cleanup-bypass"} {
		t.Run(kind, func(t *testing.T) {
			modified := mutateRunVMI(t, source, func(wf *wfv1.Workflow) {
				phases := runVMITemplate(wf, "phases")
				switch kind {
				case "create-retry":
					runVMITemplate(wf, "create-a").RetryStrategy = &wfv1.RetryStrategy{Limit: &intstr.IntOrString{IntVal: 1}}
				case "delete-retry":
					runVMITemplate(wf, "delete-a").RetryStrategy = &wfv1.RetryStrategy{Limit: &intstr.IntOrString{IntVal: 1}}
				case "unbounded-get-retry":
					runVMITemplate(wf, "address-a").RetryStrategy = &wfv1.RetryStrategy{}
				case "fanout-items":
					phases.DAG.Tasks[0].WithItems = []wfv1.Item{{Value: []byte(`1`)}}
				case "fanout-param":
					phases.DAG.Tasks[0].WithParam = "{{workflow.parameters.items}}"
				case "fanout-sequence":
					phases.DAG.Tasks[0].WithSequence = &wfv1.Sequence{Count: &intstr.IntOrString{IntVal: 2}}
				case "cycle":
					phases.DAG.Tasks[0].Depends = "k.Succeeded"
				case "recursive":
					phases.DAG.Tasks[0].Template = "phases"
				case "oversize":
					for i := range maxVMIGraphNodes {
						phases.DAG.Tasks = append(phases.DAG.Tasks, wfv1.DAGTask{Name: fmt.Sprintf("extra-%d", i), Template: "address-a"})
					}
				case "expanded-lifetime-oversize":
					main := runVMITemplate(wf, "main")
					main.DAG.Tasks = nil
					for i := range 16 {
						main.DAG.Tasks = append(main.DAG.Tasks, wfv1.DAGTask{Name: fmt.Sprintf("phase-%d", i), Template: "phases"})
					}
				case "omitted-cleanup-bypass":
					for i := range phases.DAG.Tasks {
						task := &phases.DAG.Tasks[i]
						if strings.HasPrefix(task.Template, "delete-") {
							name := strings.Split(strings.TrimPrefix(task.Depends, "("), ".")[0]
							task.Depends = "(" + name + ".Succeeded || " + name + ".Failed || " + name + ".Errored || " + name + ".Skipped || " + name + ".Omitted)"
						}
					}
				case "unsupported-status":
					phases.DAG.Tasks[4].Depends = "a.Failed || b.Succeeded"
				case "continue-on":
					phases.DAG.Tasks[0].ContinueOn = &wfv1.ContinueOn{Failed: true}
				case "exit-create":
					wf.Spec.OnExit = "create-a"
				}
			})
			r, err := ResolveRunSource(modified, fragments, binding)
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.demand()
			sum, sumErr := SummedVMIDemand(r.Workflow())
			if err != nil || sumErr != nil || got != sum {
				t.Fatalf("%s got=%+v sum=%+v err=%v/%v", kind, got, sum, err, sumErr)
			}
		})
	}
}

func TestRunVMIDeletionRequiresSuccess(t *testing.T) {
	for _, kind := range []string{"success-control", "terminal-is-only-order", "failed", "skipped", "omitted", "conditional", "unknown-guard", "create-completed", "not-ancestor", "on-exit-only"} {
		t.Run(kind, func(t *testing.T) {
			wf := phaseWorkflow([]wfv1.DAGTask{vmTask("a", "create-0"), vmTask("da", "delete-0", "a"), vmTask("b", "create-1", "da"), vmTask("db", "delete-1", "b"), vmTask("c", "create-2", "db")})
			dag := wf.Spec.Templates[len(wf.Spec.Templates)-1].DAG
			for i := range dag.Tasks {
				ds := dag.Tasks[i].Dependencies
				dag.Tasks[i].Dependencies = nil
				for _, d := range ds {
					dag.Tasks[i].Depends = d + ".Succeeded"
				}
			}
			for _, i := range []int{2, 4} {
				previous := []string{"da", "db"}[(i-2)/2]
				switch kind {
				case "terminal-is-only-order":
					dag.Tasks[i].Depends = "(" + previous + ".Succeeded || " + previous + ".Failed || " + previous + ".Errored || " + previous + ".Skipped || " + previous + ".Omitted)"
				case "failed":
					dag.Tasks[i].Depends = previous + ".Failed"
				case "skipped":
					dag.Tasks[i].Depends = previous + ".Skipped"
				case "omitted":
					dag.Tasks[i].Depends = previous + ".Omitted"
				case "conditional":
					dag.Tasks[i-1].When = "{{workflow.parameters.oberth-trigger}} == schedule"
				case "unknown-guard":
					dag.Tasks[i-1].When = "{{workflow.parameters.maybe}} == yes"
				case "create-completed", "not-ancestor", "on-exit-only":
					dag.Tasks[i].Depends = []string{"a", "b"}[(i-2)/2] + ".Succeeded"
				}
			}
			if kind == "on-exit-only" {
				wf.Spec.OnExit = "delete-0"
			}
			peak, ok := runVMIPeak(wf, RunTriggerBranch)
			if !ok {
				var err error
				peak, err = SummedVMIDemand(wf)
				if err != nil {
					t.Fatal(err)
				}
			}
			want := 3
			if kind == "success-control" {
				want = 1
			}
			if peak.VMs != want {
				t.Fatalf("%s gave release credits: peak=%+v supported=%v", kind, peak, ok)
			}
		})
	}
}

func TestRunVMIReceiptBindsSourceTriggerAndFragments(t *testing.T) {
	source, fragments, binding, policy := runVMIFixture(t)
	r, err := ResolveRunSource(source, fragments, binding)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := r.Receipt(policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"source", "trigger", "run-id", "repository", "commit", "file", "fragment-content", "fragment-commit"} {
		t.Run(kind, func(t *testing.T) {
			s := append([]byte(nil), source...)
			fs := cloneVMIFragments(fragments)
			b := binding
			key := FragmentKey{Repo: "example/driver", Version: "v1"}
			f := fs[key]
			switch kind {
			case "source":
				s = append(s, '\n')
			case "trigger":
				b.Trigger = RunTriggerPromotion
			case "run-id":
				b.RunID += "-other"
			case "repository":
				b.RepositoryID++
			case "commit":
				b.SHA = strings.Repeat("c", 40)
			case "file":
				b.File = ReleaseFile
			case "fragment-content":
				f.Source = append(f.Source, '\n')
				digest := sha256.Sum256(f.Source)
				f.Digest = hex.EncodeToString(digest[:])
				fs[key] = f
			case "fragment-commit":
				f.SHA = strings.Repeat("c", 40)
				fs[key] = f
			}
			other, err := ResolveRunSource(s, fs, b)
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := other.Receipt(policy)
			if err != nil {
				t.Fatal(err)
			}
			if err := receipt.Verify(fresh); err == nil {
				t.Fatal("changed reservation accepted")
			}
		})
	}
	for _, kind := range []string{"missing-fragment", "wrong-digest", "wrong-key", "missing-sha", "hidden-create"} {
		t.Run(kind, func(t *testing.T) {
			fs := cloneVMIFragments(fragments)
			key := FragmentKey{Repo: "example/driver", Version: "v1"}
			f := fs[key]
			switch kind {
			case "missing-fragment":
				delete(fs, key)
			case "wrong-digest":
				f.Digest = strings.Repeat("0", 64)
				fs[key] = f
			case "wrong-key":
				f.Key.Version = "v2"
				fs[key] = f
			case "missing-sha":
				f.SHA = ""
				fs[key] = f
			case "hidden-create":
				fragment, err := Decode(f.Source)
				if err != nil {
					t.Fatal(err)
				}
				vm := runVMITemplate(r.Workflow(), "create-a").DeepCopy()
				vm.Name = "hidden"
				fragment.Spec.Templates = append(fragment.Spec.Templates, *vm)
				f.Source, err = json.Marshal(fragment)
				if err != nil {
					t.Fatal(err)
				}
				f.Digest = ""
				fs[key] = f
			}
			other, err := ResolveRunSource(source, fs, binding)
			if err == nil {
				_, err = other.Receipt(policy)
			}
			if err == nil {
				t.Fatal("untrusted fragment received a demand receipt")
			}
		})
	}
	// Returned maps and byte slices cannot mutate the immutable resolution.
	fs := receipt.Fragments()
	for key, f := range fs {
		f.Source[0] = '!'
		fs[key] = f
	}
	other, err := ResolveRunSource(source, receipt.Fragments(), binding)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := other.Receipt(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.Verify(fresh); err != nil {
		t.Fatal(err)
	}
}

func TestRunVMITriggerSpoofingAndMissingContext(t *testing.T) {
	source, fragments, binding, policy := runVMIFixture(t)
	// The fixture claims tag in YAML. Branch still executes its two-VM branch
	// shape; the claim cannot disable those creates before reservation.
	r, err := ResolveRunSource(source, fragments, binding)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := r.Receipt(policy)
	if err != nil || receipt.Summary().Demand.VMs != 2 {
		t.Fatalf("unexpected receipt or error: %v", err)
	}
	for _, kind := range []string{"trigger", "run", "repository", "sha", "file"} {
		t.Run("missing-"+kind, func(t *testing.T) {
			b := binding
			switch kind {
			case "trigger":
				b.Trigger = ""
			case "run":
				b.RunID = ""
			case "repository":
				b.RepositoryID = 0
			case "sha":
				b.SHA = ""
			case "file":
				b.File = ""
			}
			r, err := ResolveRunSource(source, fragments, b)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := r.Receipt(policy)
			if err != nil || receipt.Summary().Demand.VMs != 4 || receipt.Summary().Valid() {
				t.Fatalf("missing context earned credits: %v", err)
			}
		})
	}
	modified := mutateRunVMI(t, source, func(wf *wfv1.Workflow) {
		wf.Spec.Arguments.Parameters[0].ValueFrom = &wfv1.ValueFrom{Parameter: "{{workflow.parameters.claimed}}"}
	})
	r, err = ResolveRunSource(modified, fragments, binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Receipt(policy); err == nil {
		t.Fatal("a dynamic claimed trigger was admitted")
	}
	other := r.Workflow()
	other.Spec.Entrypoint = "create-a"
	if err := Admit(other, r.Policy(policy)); err == nil || !strings.Contains(err.Error(), "does not match the admitted graph") {
		t.Fatalf("foreign graph context: %v", err)
	}
}

func TestSafeVMIPartitionGuardsUseValuesAsData(t *testing.T) {
	for _, value := range []any{"0", "1", "2", "3", "4", "5", "00", "", "unknown", "0 || true || 0", "' || true || '", "{{workflow.parameters.oberth-trigger}}", nil} {
		t.Run(fmt.Sprintf("%v", value), func(t *testing.T) {
			matched := 0
			for i := range 5 {
				guard := fmt.Sprintf("{{=inputs.parameters['partition'] == '%d'}}", i)
				encoded, err := json.Marshal(guard)
				if err != nil {
					t.Fatal(err)
				}
				replaced, err := argotemplate.Replace(argologging.TestContext(t.Context()), string(encoded), map[string]any{"inputs.parameters.partition": value}, false)
				if err != nil {
					t.Fatal(err)
				}
				var condition string
				if err := json.Unmarshal([]byte(replaced), &condition); err != nil {
					t.Fatal(err)
				}
				if condition == "true" {
					matched++
				} else if condition != "false" {
					t.Fatalf("Argo did not produce a boolean: %q", condition)
				}
			}
			want := 0
			if s, ok := value.(string); ok && len(s) == 1 && s[0] >= '0' && s[0] <= '4' {
				want = 1
			}
			if matched != want {
				t.Fatalf("immutable scalar %v matched %d partitions, want %d", value, matched, want)
			}
		})
	}
	source, fragments, binding, _ := runVMIFixture(t)
	binding.Trigger = RunTriggerSchedule
	modified := mutateRunVMI(t, source, func(wf *wfv1.Workflow) {
		for i := range runVMITemplate(wf, "partitions").DAG.Tasks {
			runVMITemplate(wf, "partitions").DAG.Tasks[i].When = fmt.Sprintf("{{inputs.parameters.partition}} == %d", i)
		}
	})
	r, err := ResolveRunSource(modified, fragments, binding)
	if err != nil {
		t.Fatal(err)
	}
	demand, err := r.demand()
	if err != nil || demand.VMs != 4 {
		t.Fatalf("raw interpolation received exclusivity credit: %+v %v", demand, err)
	}
}

func TestRunVMIProjectionBoundsCollapsedWork(t *testing.T) {
	source, fragments, binding, policy := runVMIFixture(t)
	key := FragmentKey{Repo: "example/driver", Version: "v1"}
	f := fragments[key]
	fragment, err := Decode(f.Source)
	if err != nil {
		t.Fatal(err)
	}
	leaf := *fragment.Spec.Templates[0].DeepCopy()
	leaf.Name = "leaf"
	run := wfv1.Template{Name: "run", DAG: &wfv1.DAGTemplate{}}
	for i := range MaxDAGTasks {
		run.DAG.Tasks = append(run.DAG.Tasks, wfv1.DAGTask{Name: fmt.Sprintf("leaf-%d", i), Template: "leaf"})
	}
	fragment.Spec.Templates = []wfv1.Template{run, leaf}
	f.Source, err = json.Marshal(fragment)
	if err != nil {
		t.Fatal(err)
	}
	f.Digest = ""
	fragments[key] = f
	r, err := ResolveRunSource(source, fragments, binding)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := r.Receipt(policy)
	if err != nil || receipt.Summary().Demand.VMs != 2 {
		t.Fatalf("resolved neutral graph could not collapse: %+v %v", receipt, err)
	}
	// More than 4,096 scan visits, even though all descendants are neutral and
	// would collapse to one projected node. The independent work bound wins.
	for i := range MaxDAGTasks {
		child := run.DeepCopy()
		child.Name = fmt.Sprintf("child-%d", i)
		fragment.Spec.Templates = append(fragment.Spec.Templates, *child)
		fragment.Spec.Templates[0].DAG.Tasks[i].Template = child.Name
	}
	f.Source, err = json.Marshal(fragment)
	if err != nil {
		t.Fatal(err)
	}
	fragments[key] = f
	r, err = ResolveRunSource(source, fragments, binding)
	if err != nil {
		t.Fatal(err)
	}
	demand, err := r.demand()
	if err != nil || demand.VMs != 4 {
		t.Fatalf("collapsed work bypassed independent bound: %+v %v", demand, err)
	}
}

func BenchmarkRunVMILifetimeProjection(b *testing.B) {
	source, fragments, binding, _ := runVMIFixture(b)
	r, err := ResolveRunSource(source, fragments, binding)
	if err != nil {
		b.Fatal(err)
	}
	for _, trigger := range []RunTrigger{RunTriggerBranch, RunTriggerSchedule} {
		b.Run(string(trigger), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				peak, ok := runVMIPeak(r.workflow, trigger)
				if !ok || peak.VMs != 2 {
					b.Fatal(peak, ok)
				}
			}
		})
	}
}
