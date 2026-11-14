package argojob

import (
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

var credentialHookOutputCases = []struct {
	name, key, alias string
	onExit           bool
}{
	{name: "plain", key: "scan", alias: "scan"},
	{name: "onExit", alias: "onExit", onExit: true},
	{name: "exit", key: "exit", alias: "onExit"},
	{name: "dotted", key: "x.scan", alias: "scan"},
	{name: "parenthesized", key: "scan(0)", alias: "scan"},
	{name: "bracketed", key: "scan[0]", alias: "scan"},
	{name: "expanded-retry", key: "x.scan(0:1)(0)[0]", alias: "scan"},
	{name: "dot-in-parentheses", key: "scan(0:foo.bar[)", alias: "scan"},
}

func credentialHookOutputRequest(t *testing.T, dag, onExit, capture bool, key, alias string) (Config, Request) {
	t.Helper()
	config, request := credentialOutputRequest(t, false, false, "")
	request = changeCredentialOutputRequest(t, request, func(wf *wfv1.Workflow) {
		leaf := &wf.Spec.Templates[0]
		leaf.Outputs = wfv1.Outputs{}
		credentialOtherContainer(leaf, "sidecar")
		prefix := "steps"
		if dag {
			prefix = "tasks"
		}
		reference := alias
		if !capture {
			reference = "ordinary-result"
		}
		parent := wfv1.Template{Name: "parent", Outputs: wfv1.Outputs{Parameters: []wfv1.Parameter{{Name: "result", ValueFrom: &wfv1.ValueFrom{Parameter: "{{" + prefix + "." + reference + ".outputs.result}}"}}}}}
		hooks := wfv1.LifecycleHooks{wfv1.LifecycleEvent(key): wfv1.LifecycleHook{Template: leaf.Name, Expression: "true"}}
		// The ordinary invocation owns the referenced alias. The credentialed
		// leaf is invoked only through a different sibling's hook.
		if dag {
			task := wfv1.DAGTask{Name: "host", Template: "ordinary", Hooks: hooks}
			if onExit {
				task.Hooks = nil
				task.OnExit = leaf.Name
			}
			parent.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: reference, Template: "ordinary"}, task}}
		} else {
			step := wfv1.WorkflowStep{Name: "host", Template: "ordinary", Hooks: hooks}
			if onExit {
				step.Hooks = nil
				step.OnExit = leaf.Name
			}
			parent.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: reference, Template: "ordinary"}, step}}}
		}
		wf.Spec.Entrypoint = "parent"
		wf.Spec.Templates = append(wf.Spec.Templates, parent, wfv1.Template{Name: "ordinary", Container: &corev1.Container{Image: "alpine:3.20"}})
	})
	return config, request
}

func TestCredentialHookResultCaptureRefused(t *testing.T) {
	for _, dag := range []bool{false, true} {
		for _, hook := range credentialHookOutputCases {
			t.Run(fmt.Sprintf("dag=%t/%s", dag, hook.name), func(t *testing.T) {
				config, request := credentialHookOutputRequest(t, dag, hook.onExit, true, hook.key, hook.alias)
				_, err := Build(config, request)
				if strings.ContainsAny(hook.key, ".()[]") {
					assertCredentialHookKeyRefused(t, err)
				} else if err == nil || !strings.Contains(err.Error(), "referenced outputs.result") || !strings.Contains(err.Error(), "another container of the Pod") {
					t.Fatal("hook result capture was admitted", err)
				}
			})
		}
	}
}

func TestCredentialHookUnreferencedResultAllowed(t *testing.T) {
	for _, dag := range []bool{false, true} {
		for _, hook := range credentialHookOutputCases {
			if strings.ContainsAny(hook.key, ".()[]") {
				continue
			}
			t.Run(fmt.Sprintf("dag=%t/%s", dag, hook.name), func(t *testing.T) {
				config, request := credentialHookOutputRequest(t, dag, hook.onExit, false, hook.key, hook.alias)
				if _, err := Build(config, request); err != nil {
					t.Fatal("unreferenced credentialed hook was refused", err)
				}
			})
		}
	}
}

func assertCredentialHookKeyRefused(t *testing.T, err error) {
	t.Helper()
	const message = "its key cannot contain '.', '(', ')', '[' or ']'"
	if err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("credentialed hook delimiter was admitted or returned an unexpected error: %v", err)
	}
}

func TestCredentialHookDelimiterKeysRefused(t *testing.T) {
	for _, dag := range []bool{false, true} {
		for _, sidecar := range []bool{false, true} {
			for _, key := range []string{"x.scan", "scan(", "scan)", "scan[", "scan]"} {
				t.Run(fmt.Sprintf("dag=%t/sidecar=%t/key=%s", dag, sidecar, key), func(t *testing.T) {
					config, request := credentialHookOutputRequest(t, dag, false, false, key, "unused")
					if !sidecar {
						request = changeCredentialOutputRequest(t, request, func(wf *wfv1.Workflow) {
							wf.Spec.Templates[0].Sidecars = nil
						})
					}
					_, err := Build(config, request)
					assertCredentialHookKeyRefused(t, err)
					assertCredentialHookContext(t, err, dag, key)
				})
			}
		}
	}
}

func assertCredentialHookContext(t *testing.T, err error, dag bool, key string) {
	t.Helper()
	kind := "step"
	if dag {
		kind = "task"
	}
	context := fmt.Sprintf("hook %q on %s %q in template %q", key, kind, "host", "parent")
	if err == nil || !strings.Contains(err.Error(), context) {
		t.Fatalf("hook refusal did not identify its invocation (%s): %v", context, err)
	}
}

func TestCredentialHookDynamicDelimiterMessage(t *testing.T) {
	for _, dag := range []bool{false, true} {
		t.Run(fmt.Sprintf("dag=%t", dag), func(t *testing.T) {
			config, request := credentialHookOutputRequest(t, dag, false, false, "scan)", "unused")
			request = changeCredentialOutputRequest(t, request, func(wf *wfv1.Workflow) {
				for i := range wf.Spec.Templates {
					parent := &wf.Spec.Templates[i]
					if parent.Name != "parent" {
						continue
					}
					hooks := wfv1.LifecycleHooks{"scan)": {Template: "{{workflow.parameters.hook}}", Expression: "true"}}
					if dag {
						parent.DAG.Tasks[1].Hooks = hooks
					} else {
						parent.Steps[0].Steps[1].Hooks = hooks
					}
				}
			})
			_, err := Build(config, request)
			assertCredentialHookKeyRefused(t, err)
			assertCredentialHookContext(t, err, dag, "scan)")
			if !strings.Contains(err.Error(), "or a template chosen at run time") {
				t.Fatalf("dynamic hook refusal did not explain template selection: %v", err)
			}
		})
	}
}

func TestOrdinaryHookDelimiterKeysAllowed(t *testing.T) {
	for _, dag := range []bool{false, true} {
		for _, key := range []string{"x.scan", "scan(", "scan)", "scan[", "scan]"} {
			t.Run(fmt.Sprintf("dag=%t/key=%s", dag, key), func(t *testing.T) {
				config, request := credentialHookOutputRequest(t, dag, false, false, key, "unused")
				request = changeCredentialOutputRequest(t, request, func(wf *wfv1.Workflow) {
					for i := range wf.Spec.Templates {
						parent := &wf.Spec.Templates[i]
						if parent.Name != "parent" {
							continue
						}
						hooks := wfv1.LifecycleHooks{wfv1.LifecycleEvent(key): {Template: "ordinary", Expression: "true"}}
						if dag {
							parent.DAG.Tasks[1].Hooks = hooks
						} else {
							parent.Steps[0].Steps[1].Hooks = hooks
						}
					}
				})
				if _, err := Build(config, request); err != nil {
					t.Fatalf("ordinary hook with delimiter was refused: %v", err)
				}
			})
		}
	}
}

func TestCredentialHookExpandedParentDelimiterRefused(t *testing.T) {
	for _, name := range []string{"steps-expanded-paren", "dag-nested-expanded-paren", "steps-nested-bracket"} {
		t.Run(name, func(t *testing.T) {
			dag := name == "dag-nested-expanded-paren"
			key, reference := "scan)", "host"
			if name == "steps-nested-bracket" {
				key = "scan]"
			}
			if name != "steps-expanded-paren" {
				reference = "outer"
			}
			config, request := credentialHookOutputRequest(t, dag, false, true, key, reference)
			request = changeCredentialOutputRequest(t, request, func(wf *wfv1.Workflow) {
				for i := range wf.Spec.Templates {
					parent := &wf.Spec.Templates[i]
					if parent.Name != "parent" {
						continue
					}
					if name == "steps-expanded-paren" {
						parent.Steps[0].Steps = parent.Steps[0].Steps[1:]
						parent.Steps[0].Steps[0].WithParam = `["item"]`
						return
					}
					parent.Name = "inner"
					top := wfv1.Template{Name: "parent"}
					if dag {
						top.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "outer", Template: "inner", WithParam: `["item"]`}}}
					} else {
						top.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "outer", Template: "inner"}}}}
					}
					wf.Spec.Templates = append(wf.Spec.Templates, top)
					return
				}
			})
			_, err := Build(config, request)
			assertCredentialHookKeyRefused(t, err)
		})
	}
}

func TestCredentialHookExpandedParentPlainAllowed(t *testing.T) {
	config, request := credentialHookOutputRequest(t, false, false, true, "scan", "host")
	request = changeCredentialOutputRequest(t, request, func(wf *wfv1.Workflow) {
		for i := range wf.Spec.Templates {
			parent := &wf.Spec.Templates[i]
			if parent.Name == "parent" {
				parent.Steps[0].Steps = parent.Steps[0].Steps[1:]
				parent.Steps[0].Steps[0].WithParam = `["item"]`
			}
		}
	})
	if _, err := Build(config, request); err != nil {
		t.Fatalf("plain credentialed hook on an expanded parent was refused: %v", err)
	}
}

func TestCredentialHookUnmatchedAliasCaptured(t *testing.T) {
	for _, name := range []string{"steps-loop-unbalanced-paren", "dag-nested-in-loop-unbalanced-paren", "steps-nested-unbalanced-bracket"} {
		t.Run(name, func(t *testing.T) {
			dag := name == "dag-nested-in-loop-unbalanced-paren"
			key, reference := "x)", "outer"
			if name == "steps-nested-unbalanced-bracket" {
				key = "x]"
			}
			if name == "steps-loop-unbalanced-paren" {
				reference = "scan"
			}
			config, request := credentialHookOutputRequest(t, dag, false, true, key, reference)
			request = changeCredentialOutputRequest(t, request, func(wf *wfv1.Workflow) {
				for i := range wf.Spec.Templates {
					parent := &wf.Spec.Templates[i]
					if parent.Name != "parent" {
						continue
					}
					if name == "steps-loop-unbalanced-paren" {
						parent.Steps[0].Steps = parent.Steps[0].Steps[1:]
						host := &parent.Steps[0].Steps[0]
						host.Name = reference
						item, err := wfv1.ParseItem(`"a"`)
						if err != nil {
							t.Fatal(err)
						}
						host.WithItems = []wfv1.Item{item}
					} else {
						parent.Name = "inner"
						top := wfv1.Template{Name: "parent"}
						if dag {
							top.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "outer", Template: "inner", WithParam: `["a"]`}}}
						} else {
							top.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "outer", Template: "inner"}}}}
						}
						wf.Spec.Templates = append(wf.Spec.Templates, top)
					}
					break
				}
				if !credentialResultReferenced(wf, &wf.Spec.Templates[0]) {
					t.Fatal("unmatched hook closer was not treated as captured")
				}
			})
			_, err := Build(config, request)
			assertCredentialHookKeyRefused(t, err)
		})
	}
}

func TestCredentialHookExitPrecedence(t *testing.T) {
	for _, dag := range []bool{false, true} {
		for _, tc := range []struct {
			name, onExit, hook string
			refused            bool
		}{
			{name: "ordinary-onExit-shadows-credentialed-hook", onExit: "ordinary", hook: "credentialed"},
			{name: "ordinary-onExit-shadows-dynamic-hook", onExit: "ordinary", hook: "{{workflow.parameters.exit}}"},
			{name: "credentialed-onExit-shadows-ordinary-hook", onExit: "credentialed", hook: "ordinary", refused: true},
			{name: "credentialed-exit-hook", hook: "credentialed", refused: true},
			{name: "dynamic-onExit-shadows-ordinary-hook", onExit: "{{workflow.parameters.exit}}", hook: "ordinary", refused: true},
			{name: "ordinary-onExit-without-hook", onExit: "ordinary"},
		} {
			t.Run(fmt.Sprintf("dag=%t/%s", dag, tc.name), func(t *testing.T) {
				config, request := credentialHookOutputRequest(t, dag, false, true, "exit", "onExit")
				request = changeCredentialOutputRequest(t, request, func(wf *wfv1.Workflow) {
					resolve := func(name string) string {
						if name == "credentialed" {
							return wf.Spec.Templates[0].Name
						}
						return name
					}
					var hooks wfv1.LifecycleHooks
					if tc.hook != "" {
						hooks = wfv1.LifecycleHooks{wfv1.ExitLifecycleEvent: {Template: resolve(tc.hook)}}
					}
					for i := range wf.Spec.Templates {
						parent := &wf.Spec.Templates[i]
						if parent.Name == "parent" {
							if dag {
								parent.DAG.Tasks[1].OnExit, parent.DAG.Tasks[1].Hooks = resolve(tc.onExit), hooks
							} else {
								parent.Steps[0].Steps[1].OnExit, parent.Steps[0].Steps[1].Hooks = resolve(tc.onExit), hooks
							}
						}
					}
				})
				_, err := Build(config, request)
				if tc.refused {
					if err == nil || !strings.Contains(err.Error(), "referenced outputs.result") || !strings.Contains(err.Error(), "another container of the Pod") {
						t.Fatalf("active credentialed exit was admitted or returned an unexpected error: %v", err)
					}
				} else if err != nil {
					t.Fatalf("ordinary effective exit was refused: %v", err)
				}
			})
		}
	}
}

func TestCredentialHookEmptyAlias(t *testing.T) {
	for _, dag := range []bool{false, true} {
		for _, key := range []string{"", "scan"} {
			for _, form := range []string{"unreferenced", "dot", "indexed", "expression"} {
				t.Run(fmt.Sprintf("dag=%t/key=%q/%s", dag, key, form), func(t *testing.T) {
					config, request := credentialHookOutputRequest(t, dag, false, false, key, "unused")
					request = changeCredentialOutputRequest(t, request, func(wf *wfv1.Workflow) {
						prefix := "steps"
						if dag {
							prefix = "tasks"
						}
						for i := range wf.Spec.Templates {
							parent := &wf.Spec.Templates[i]
							if parent.Name != "parent" {
								continue
							}
							parent.Outputs = wfv1.Outputs{}
							if form == "unreferenced" {
								continue
							}
							reference := "{{" + prefix + "." + key + ".outputs.result}}"
							if form == "indexed" {
								reference = "{{" + prefix + "['" + key + "'].outputs.result}}"
							} else if form == "expression" {
								reference = "{{=" + prefix + "[\"" + key + "\"].outputs.result}}"
							}
							parent.Outputs.Parameters = []wfv1.Parameter{{Name: "result", ValueFrom: &wfv1.ValueFrom{Parameter: reference}}}
						}
					})
					_, err := Build(config, request)
					if form == "unreferenced" {
						if err != nil {
							t.Fatalf("unreferenced credentialed hook was refused: %v", err)
						}
					} else if err == nil || !strings.Contains(err.Error(), "referenced outputs.result") {
						t.Fatalf("referenced credentialed hook was admitted or returned an unexpected error: %v", err)
					}
				})
			}
		}
	}
}
