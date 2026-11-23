package main

import "go/ast"

// reasonFormatter is the one call allowed to give a status reason: the native
// lock reason, which fills catalogue formats with a plain name and falls back
// to its general phrase beyond 40 characters (internal/argojob/waiting.go).
func reasonFormatter(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	name, ok := call.Fun.(*ast.Ident)
	return ok && name.Name == "lockReason"
}
