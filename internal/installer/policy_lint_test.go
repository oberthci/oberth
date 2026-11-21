package installer_test

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	expr "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

func guardedExpression(text string) string {
	env, err := cel.NewEnv()
	if err != nil {
		return err.Error()
	}
	ast, issues := env.Parse(text)
	if issues != nil && issues.Err() != nil {
		return issues.String()
	}
	parsed, err := cel.AstToParsedExpr(ast)
	if err != nil {
		return err.Error()
	}
	if err = walkPolicy(parsed.Expr, map[string]bool{}); err != nil {
		return err.Error()
	}
	return ""
}
func path(e *expr.Expr) string {
	if id := e.GetIdentExpr(); id != nil {
		return id.Name
	}
	if s := e.GetSelectExpr(); s != nil {
		return path(s.Operand) + "." + s.Field
	}
	if c := e.GetCallExpr(); c != nil && c.Function == "_[_]" {
		return path(c.Args[0]) + "[" + c.Args[1].GetConstExpr().GetStringValue() + "]"
	}
	return ""
}
func extended(guards map[string]bool, e *expr.Expr, truth bool) map[string]bool {
	copy := map[string]bool{}
	for k, v := range guards {
		copy[k] = v
	}
	for k, v := range proven(e, truth) {
		copy[k] = v
	}
	return copy
}
func proven(e *expr.Expr, truth bool) map[string]bool {
	if s := e.GetSelectExpr(); s != nil && s.TestOnly && truth {
		return map[string]bool{path(e): true}
	}
	if c := e.GetCallExpr(); c != nil {
		return callProof(c, truth)
	}
	return nil
}
func callProof(c *expr.Expr_Call, truth bool) map[string]bool {
	switch c.Function {
	case "!_":
		return proven(c.Args[0], !truth)
	case "_&&_":
		if truth {
			return extended(proven(c.Args[0], true), c.Args[1], true)
		}
	case "_||_":
		if !truth {
			return extended(proven(c.Args[0], false), c.Args[1], false)
		}
	case "_!=_", "_==_":
		if truth == (c.Function == "_!=_") && nullComparison(c) {
			return map[string]bool{path(c.Args[0]): true}
		}
	case "@in", "_in_":
		if truth {
			return map[string]bool{path(c.Args[1]) + "[" + c.Args[0].GetConstExpr().GetStringValue() + "]": true}
		}
	}
	return nil
}
func nullComparison(c *expr.Expr_Call) bool {
	if (path(c.Args[0]) != "oldObject" && path(c.Args[0]) != "r") || c.Args[1].GetConstExpr() == nil {
		return false
	}
	_, ok := c.Args[1].GetConstExpr().ConstantKind.(*expr.Constant_NullValue)
	return ok
}
func optional(name string) bool {
	// These API fields are required; nullable objects are checked at selection.
	if name == "request.operation" || name == "request.userInfo" || name == "request.userInfo.username" || name == "object.metadata" || name == "oldObject.metadata" || name == "r.metadata" {
		return false
	}
	for _, prefix := range []string{"request.", "object.", "oldObject.", "r."} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
func walkPolicy(e *expr.Expr, guards map[string]bool) error {
	if e == nil {
		return nil
	}
	switch node := e.ExprKind.(type) {
	case *expr.Expr_SelectExpr:
		return walkPolicySelect(e, node.SelectExpr, guards)
	case *expr.Expr_CallExpr:
		return walkPolicyCall(node.CallExpr, guards)
	case *expr.Expr_ListExpr:
		return walkPolicyNodes(node.ListExpr.Elements, guards)
	case *expr.Expr_ComprehensionExpr:
		c := node.ComprehensionExpr
		return walkPolicyNodes([]*expr.Expr{c.IterRange, c.AccuInit, c.LoopCondition, c.LoopStep, c.Result}, guards)
	}
	return nil
}
func walkPolicySelect(e *expr.Expr, s *expr.Expr_Select, guards map[string]bool) error {
	root := path(s.Operand)
	if (root == "oldObject" || root == "r") && !guards[root] {
		return fmt.Errorf("unguarded nullable object %s", root)
	}
	if !s.TestOnly && optional(path(e)) && !guards[path(e)] {
		return fmt.Errorf("unguarded optional field %s", path(e))
	}
	return walkPolicy(s.Operand, guards)
}
func walkPolicyNodes(nodes []*expr.Expr, guards map[string]bool) error {
	for _, node := range nodes {
		if err := walkPolicy(node, guards); err != nil {
			return err
		}
	}
	return nil
}
func walkPolicyCall(c *expr.Expr_Call, guards map[string]bool) error {
	switch c.Function {
	case "_!=_", "_==_":
		if nullComparison(c) {
			return nil
		}
	case "_&&_", "_||_":
		if err := walkPolicy(c.Args[0], guards); err != nil {
			return err
		}
		return walkPolicy(c.Args[1], extended(guards, c.Args[0], c.Function == "_&&_"))
	case "_?_:_":
		return walkPolicyConditional(c, guards)
	case "_[_]":
		if err := checkPolicyIndex(c, guards); err != nil {
			return err
		}
	}
	return walkPolicyNodes(append([]*expr.Expr{c.Target}, c.Args...), guards)
}
func walkPolicyConditional(c *expr.Expr_Call, guards map[string]bool) error {
	if err := walkPolicy(c.Args[0], guards); err != nil {
		return err
	}
	if err := walkPolicy(c.Args[1], extended(guards, c.Args[0], true)); err != nil {
		return err
	}
	return walkPolicy(c.Args[2], extended(guards, c.Args[0], false))
}
func checkPolicyIndex(c *expr.Expr_Call, guards map[string]bool) error {
	base := path(c.Args[0])
	if !strings.HasSuffix(base, ".labels") && !strings.HasSuffix(base, ".extra") {
		return nil
	}
	key := base + "[" + c.Args[1].GetConstExpr().GetStringValue() + "]"
	if !guards[key] {
		return fmt.Errorf("unguarded optional map entry %s", key)
	}
	return nil
}
