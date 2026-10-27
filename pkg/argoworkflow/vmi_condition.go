package argoworkflow

import (
	"regexp"
	"strings"
)

// This is a closed expression grammar, not an Argo/govaluate interpreter.
// Unknown creates remain enabled; unknown deletes never become barriers.
type vmiTruth uint8

const (
	vmiUnknown vmiTruth = iota
	vmiFalse
	vmiTrue
)

type vmiScalar struct {
	id, literal string
	constant    bool
}
type vmiCondition struct {
	truth           vmiTruth
	scalar, literal string
	unsafe          bool
}

var vmiScalarReference = regexp.MustCompile(`^\{\{\s*(workflow\.parameters\.oberth-trigger|inputs\.parameters\.[a-zA-Z0-9_-]+|(?:tasks|steps)\.[a-z0-9-]+\.outputs\.parameters\.[a-zA-Z0-9_-]+)\s*\}\}$`)
var vmiGuardLiteral = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Only expression-tag comparisons read output values as data. Raw {{...}}
// interpolation into govaluate's source can inject syntax and is NOT an
// exclusivity proof, even if every reference spells the same output name.
var vmiSafePartitionGuard = regexp.MustCompile(`^\{\{=\s*(inputs\.parameters\['([a-zA-Z0-9_-]+)'\]|(?:tasks|steps)\['([a-z0-9-]+)'\]\.outputs\.parameters\['([a-zA-Z0-9_-]+)'\])\s*==\s*'([a-zA-Z0-9_-]{1,64})'\s*\}\}$`)

func vmiUnquote(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) > 1 && (s[0] == '\'' || s[0] == '"') {
		if s[len(s)-1] != s[0] {
			return "", false
		}
		s = s[1 : len(s)-1]
	}
	return s, !strings.ContainsAny(s, "'\"\\")
}

// splitVMIExpression ignores operators inside quoted operands and only splits
// at depth zero. Parenthesis/expression bounds are independent of graph bounds.
func splitVMIExpression(s, operator string) ([]string, bool) {
	var parts []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth < 0 || depth > 16 {
			return nil, false
		}
		if depth == 0 && strings.HasPrefix(s[i:], operator) {
			parts = append(parts, strings.TrimSpace(s[start:i]))
			i += len(operator) - 1
			start = i + 1
		}
	}
	if depth != 0 || quote != 0 {
		return nil, false
	}
	return append(parts, strings.TrimSpace(s[start:])), true
}

// vmiCombine also joins independent complete guards (parent and child). For
// pieces of one expression, the parser must reject unsafe operands FIRST.
func vmiCombine(a, b vmiCondition, and bool) vmiCondition {
	if and {
		if a.truth == vmiFalse || b.truth == vmiFalse {
			return vmiCondition{truth: vmiFalse}
		}
		if a.truth == vmiTrue {
			return b
		}
		if b.truth == vmiTrue {
			return a
		}
	} else {
		if a.truth == vmiTrue || b.truth == vmiTrue {
			return vmiCondition{truth: vmiTrue}
		}
		if a.truth == vmiFalse {
			return b
		}
		if b.truth == vmiFalse {
			return a
		}
	}
	// Only one literal equality may survive trigger folding. Arbitrary output
	// expressions, including ORs of partition selectors, earn no exclusivity.
	if a.scalar != "" && a == b {
		return a
	}
	return vmiCondition{truth: vmiUnknown, unsafe: true}
}

func parseVMICondition(s string, trigger RunTrigger, resolve func(string) (vmiScalar, bool), depth int) vmiCondition {
	if len(s) > 4096 || depth > 16 {
		return vmiCondition{unsafe: true}
	}
	s = strings.TrimSpace(s)
	if s == "" {
		if depth > 0 {
			return vmiCondition{unsafe: true}
		}
		return vmiCondition{truth: vmiTrue}
	}
	if s == "true" {
		return vmiCondition{truth: vmiTrue}
	}
	if s == "false" {
		return vmiCondition{truth: vmiFalse}
	}
	if match := vmiSafePartitionGuard.FindStringSubmatch(s); len(match) == 6 {
		ref := "inputs.parameters." + match[2]
		if match[2] == "" {
			kind := "tasks"
			if strings.HasPrefix(match[1], "steps[") {
				kind = "steps"
			}
			ref = kind + "." + match[3] + ".outputs.parameters." + match[4]
		}
		scalar, ok := resolve(ref)
		if !ok {
			return vmiCondition{unsafe: true}
		}
		if scalar.constant {
			if scalar.literal == match[5] {
				return vmiCondition{truth: vmiTrue}
			}
			return vmiCondition{truth: vmiFalse}
		}
		return vmiCondition{scalar: scalar.id, literal: match[5]}
	}
	if s[0] == '(' && s[len(s)-1] == ')' {
		if _, ok := splitVMIExpression(s[1:len(s)-1], "||"); ok {
			return parseVMICondition(s[1:len(s)-1], trigger, resolve, depth+1)
		}
	}
	for _, operator := range []string{"||", "&&"} {
		parts, ok := splitVMIExpression(s, operator)
		if !ok {
			return vmiCondition{unsafe: true}
		}
		if len(parts) > 1 {
			result := parseVMICondition(parts[0], trigger, resolve, depth+1)
			for _, part := range parts[1:] {
				operand := parseVMICondition(part, trigger, resolve, depth+1)
				// These pieces belong to ONE authored expression. An unknown
				// piece may contain a looser-precedence ternary/coalesce (or
				// raw interpolation), so boolean folding cannot discard it.
				if result.unsafe || operand.unsafe {
					return vmiCondition{unsafe: true}
				}
				result = vmiCombine(result, operand, operator == "&&")
			}
			return result
		}
	}
	operator := "=="
	left, right, ok := strings.Cut(s, operator)
	if !ok {
		operator = "!="
		left, right, ok = strings.Cut(s, operator)
	}
	if !ok {
		return vmiCondition{unsafe: true}
	}
	left, leftOK := vmiUnquote(left)
	right, rightOK := vmiUnquote(right)
	ref := vmiScalarReference.FindStringSubmatch(left)
	if !leftOK || !rightOK || len(ref) != 2 || !vmiGuardLiteral.MatchString(right) {
		return vmiCondition{unsafe: true}
	}
	if ref[1] == "workflow.parameters."+TriggerParameter {
		if !trigger.Valid() || !RunTrigger(right).Valid() {
			return vmiCondition{unsafe: true}
		}
		if (string(trigger) == right) == (operator == "==") {
			return vmiCondition{truth: vmiTrue}
		}
		return vmiCondition{truth: vmiFalse}
	}
	return vmiCondition{unsafe: true}
}

func (c vmiCondition) evaluate(selector, value string) vmiTruth {
	if c.scalar == "" {
		return c.truth
	}
	if c.scalar != selector {
		return vmiUnknown
	}
	if c.literal == value {
		return vmiTrue
	}
	return vmiFalse
}

type vmiDependency struct {
	name          string
	success, bare bool
	executed      bool
}

// Precisely recognize conjunctions of successful completion and terminal-state
// OR groups for ONE predecessor. Terminal groups establish order, not success.
// No negation, Daemoned, AnySucceeded, mixed-task OR, or arbitrary status logic.
// Mixed top-level &&/|| is refused: Argo binds && more tightly, so treating
// each && operand as an OR group could invent a successful delete prerequisite.
func parseVMIDependencies(expression string) ([]vmiDependency, bool) {
	if len(expression) > 4096 {
		return nil, false
	}
	parts, ok := splitVMIExpression(strings.TrimSpace(expression), "&&")
	if !ok {
		return nil, false
	}
	if ors, valid := splitVMIExpression(strings.TrimSpace(expression), "||"); !valid || (len(parts) > 1 && len(ors) > 1) {
		return nil, false
	}
	var out []vmiDependency
	for _, part := range parts {
		for len(part) > 1 && part[0] == '(' && part[len(part)-1] == ')' {
			inside := strings.TrimSpace(part[1 : len(part)-1])
			if _, ok := splitVMIExpression(inside, "||"); !ok {
				break
			}
			part = inside
		}
		terms, ok := splitVMIExpression(part, "||")
		if !ok {
			return nil, false
		}
		name, statuses := "", map[string]bool{}
		for _, term := range terms {
			task, status, found := strings.Cut(strings.TrimSpace(term), ".")
			if !vmiDNSLabelPattern.MatchString(task) || (name != "" && task != name) || statuses[status] {
				return nil, false
			}
			if !found {
				status = ""
			}
			name = task
			statuses[status] = true
		}
		if len(terms) == 1 && (statuses["Succeeded"] || statuses[""]) {
			out = append(out, vmiDependency{name: name, success: statuses["Succeeded"], bare: statuses[""], executed: statuses["Succeeded"]})
			continue
		}
		if !statuses["Succeeded"] || !statuses["Failed"] || !statuses["Errored"] {
			return nil, false
		}
		if len(statuses) != 3 && (len(statuses) != 5 || !statuses["Skipped"] || !statuses["Omitted"]) {
			return nil, false
		}
		out = append(out, vmiDependency{name: name, executed: len(statuses) == 3})
	}
	return out, true
}
