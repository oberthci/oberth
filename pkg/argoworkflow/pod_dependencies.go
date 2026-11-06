package argoworkflow

import (
	"regexp"
	"sort"
	"strings"
)

// podCompletionDependencies proves which predecessors have completed whenever a
// dependency expression is true. AND unions prerequisites; OR intersects them.
// Only terminal states earn ordering credit. This proof concerns ordinary pod
// overlap.
func podCompletionDependencies(expression string) ([]string, bool) {
	if len(expression) > 4096 {
		return nil, false
	}
	var walk func(string, int) (map[string]bool, bool)
	walk = func(expression string, depth int) (map[string]bool, bool) {
		if depth > 16 {
			return nil, false
		}
		expression = strings.TrimSpace(expression)
		ors, ok := splitDependencyExpression(expression, "||")
		if !ok {
			return nil, false
		}
		ands, ok := splitDependencyExpression(expression, "&&")
		if !ok {
			return nil, false
		}
		// Keep ambiguous mixed top-level syntax conservative. Explicit grouping
		// avoids silently inventing precedence credits for unsupported expressions.
		if len(ors) > 1 && len(ands) > 1 {
			return nil, false
		}
		parts := ands
		union := true
		if len(ors) > 1 {
			parts = ors
			union = false
		}
		if len(parts) > 1 {
			var result map[string]bool
			for i, part := range parts {
				names, ok := walk(part, depth+1)
				if !ok {
					return nil, false
				}
				if i == 0 {
					result = names
					continue
				}
				if union {
					for name := range names {
						result[name] = true
					}
				} else {
					for name := range result {
						if !names[name] {
							delete(result, name)
						}
					}
				}
			}
			return result, true
		}
		if len(expression) > 1 && expression[0] == '(' && expression[len(expression)-1] == ')' {
			return walk(expression[1:len(expression)-1], depth+1)
		}
		name, status, qualified := strings.Cut(expression, ".")
		if qualified && status == "" {
			return nil, false
		}
		if !dependencyNamePattern.MatchString(name) {
			return nil, false
		}
		switch status {
		case "", "Succeeded", "Failed", "Errored", "Skipped", "Omitted":
			return map[string]bool{name: true}, true
		default:
			return nil, false
		}
	}
	names, ok := walk(expression, 0)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, true
}

var dependencyNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// splitDependencyExpression ignores operators inside quoted operands and only splits
// at depth zero. Parenthesis/expression bounds are independent of graph bounds.
func splitDependencyExpression(s, operator string) ([]string, bool) {
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
