package secretstore

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSATokenStringConversionGuard walks the non-test source files of this
// package and asserts that the only string(token) conversion on the SA token
// path is the documented site inside login (the vault API boundary). No
// string(raw) conversion may appear in serviceAccountToken — that function
// must operate on []byte end to end.
func TestSATokenStringConversionGuard(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	// Collect string(raw) conversions in serviceAccountToken and
	// string(token) conversions in login.
	var rawInSAToken []string
	var tokenInLogin []string

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if !ok || funcDecl.Name == nil {
				continue
			}
			funcName := funcDecl.Name.Name
			if funcName != "serviceAccountToken" && funcName != "login" {
				continue
			}
			ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) != 1 {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok || ident.Name != "string" {
					return true
				}
				var buf bytes.Buffer
				_ = printer.Fprint(&buf, fset, call.Args[0])
				arg := buf.String()
				pos := fset.Position(call.Pos())
				site := filepath.Base(pos.Filename) + ":" + arg
				if funcName == "serviceAccountToken" && arg == "raw" {
					rawInSAToken = append(rawInSAToken, site)
				}
				if funcName == "login" && arg == "token" {
					tokenInLogin = append(tokenInLogin, site)
				}
				return true
			})
		}
	}

	if len(rawInSAToken) != 0 {
		t.Fatalf("serviceAccountToken must not contain string(raw) — operate on []byte; found %d: %v", len(rawInSAToken), rawInSAToken)
	}
	if len(tokenInLogin) != 1 {
		t.Fatalf("expected exactly 1 documented string(token) in login (vault API boundary), found %d: %v", len(tokenInLogin), tokenInLogin)
	}
}

// TestServiceAccountTokenReturnsIndependentCopy verifies that the []byte
// returned by serviceAccountToken is an independent allocation. Clearing it
// does not corrupt subsequent reads — proving the internal file-read buffer
// (raw) was cloned, not returned as a subslice.
func TestServiceAccountTokenReturnsIndependentCopy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	expected := "test-service-account-jwt-value"
	if err := os.WriteFile(tokenPath, []byte(expected+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &Client{tokenPath: tokenPath}

	token, err := client.serviceAccountToken(context.Background())
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if string(token) != expected {
		t.Fatalf("token = %q, want %q", token, expected)
	}

	// Zero the returned value — must not affect subsequent reads.
	clear(token)
	for _, b := range token {
		if b != 0 {
			t.Fatal("clear(token) did not zero the returned buffer")
		}
	}

	token2, err := client.serviceAccountToken(context.Background())
	if err != nil {
		t.Fatalf("second read after zeroing: %v", err)
	}
	if string(token2) != expected {
		t.Fatalf("second read = %q, want %q (zeroing first result corrupted state)", token2, expected)
	}
}
