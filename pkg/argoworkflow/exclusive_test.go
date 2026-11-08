package argoworkflow

import (
	"reflect"
	"strings"
	"testing"
)

const exclusiveDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      dag:
        tasks:
          - name: vm
            template: vm
          - name: inline
            inline:
              metadata:
                annotations:
                  oberth.ci/exclusive: gpu-lab
              container:
                image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
                command: [/bin/true]
    - name: vm
      metadata:
        annotations:
          oberth.ci/exclusive: kvm
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`

func TestDeclaredExclusiveClassesReadsStepTemplates(t *testing.T) {
	mustAdmit(t, exclusiveDocument, Policy{})
	workflow, err := Decode([]byte(exclusiveDocument))
	if err != nil {
		t.Fatal(err)
	}
	classes, err := DeclaredExclusiveClasses(workflow)
	if err != nil || !reflect.DeepEqual(classes, []string{"gpu-lab", "kvm"}) {
		t.Fatalf("classes %v %v", classes, err)
	}
	none, err := Decode([]byte(admissibleDocument))
	if err != nil {
		t.Fatal(err)
	}
	if classes, err := DeclaredExclusiveClasses(none); err != nil || len(classes) != 0 {
		t.Fatalf("undeclared document has classes %v %v", classes, err)
	}
}

func TestExclusiveClassSyntaxIsBounded(t *testing.T) {
	for _, bad := range []string{"", "-kvm", "kvm-", "KVM", "kvm/x", "exclusive/kvm", "kvm x", strings.Repeat("a", MaxExclusiveClassLength+1)} {
		if ValidExclusiveClass(bad) {
			t.Fatalf("accepted %q", bad)
		}
		document := strings.Replace(exclusiveDocument, "oberth.ci/exclusive: kvm", "oberth.ci/exclusive: \""+bad+"\"", 1)
		refuse(t, document, Policy{}, "must name an exclusive class")
	}
	many := exclusiveDocument
	for _, class := range []string{"a", "b", "c", "d"} {
		many += "    - name: vm-" + class + "\n      metadata:\n        annotations:\n          oberth.ci/exclusive: " + class + "\n      container:\n        image: x\n"
	}
	workflow, err := Decode([]byte(many))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DeclaredExclusiveClasses(workflow); err == nil || !strings.Contains(err.Error(), "at most 4") {
		t.Fatalf("unbounded class count accepted: %v", err)
	}
}

func TestParseExclusiveClasses(t *testing.T) {
	classes, err := ParseExclusiveClasses(" kvm , gpu-lab=2 ")
	if err != nil || !reflect.DeepEqual(classes, map[string]int{"kvm": 1, "gpu-lab": 2}) {
		t.Fatalf("parsed %v %v", classes, err)
	}
	if empty, err := ParseExclusiveClasses(""); err != nil || len(empty) != 0 {
		t.Fatal("empty allowlist must parse as feature off", empty, err)
	}
	for _, bad := range []string{"kvm=0", "kvm=65", "kvm=+1", "kvm=x", "kvm,kvm", "Bad", "kvm=1,", "a/b"} {
		if _, err := ParseExclusiveClasses(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
