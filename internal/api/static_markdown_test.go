package api

import (
	"strings"
	"testing"
)

// TestIssueBodyMarkdownRendererInvariants pins the security-relevant structure
// of the dashboard's issue-body Markdown renderer (OBERTH-RELEASE-001). The
// renderer is escape-first: every text path must pass esc() before any
// transform, links must be scheme-allowlisted, and generated anchors must be
// hardened. Behavioral verification of the six XSS vectors is DOM-free and
// string-level (see the PR notes); this test keeps the structural properties
// from silently regressing in the served asset.
func TestIssueBodyMarkdownRendererInvariants(t *testing.T) {
	script, ok := staticAssets["app.js"]
	if !ok {
		t.Fatal("app.js is not embedded")
	}
	source := string(script.body)

	for _, required := range []string{
		// The renderer exists as a pure function and the dialog uses it for the
		// body instead of raw escaped text.
		"function renderMarkdown(raw)",
		"renderMarkdown(issue.Body)",
		// Positive scheme allowlist: only http(s) URLs ever become anchors.
		`function mdSafeHref(url) { return /^https?:\/\//i.test(url); }`,
		// Generated anchors carry exactly the hardened attribute set.
		`target="_blank" rel="noopener noreferrer"`,
		// Escape-first on all three text paths: fenced blocks, block-level
		// prose (headers and list items), and paragraph lines.
		"esc(block.join",
		"mdInline(esc(",
		"paragraph.push(esc(line))",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("app.js is missing the renderer invariant %q", required)
		}
	}

	// The issue dialog must not fall back to injecting the raw body.
	if strings.Contains(source, "${esc(issue.Body") {
		t.Error("issue dialog renders the raw escaped body instead of renderMarkdown")
	}

	style, ok := staticAssets["app.css"]
	if !ok {
		t.Fatal("app.css is not embedded")
	}
	for _, required := range []string{".issue-body.md", "pre.md-code"} {
		if !strings.Contains(string(style.body), required) {
			t.Errorf("app.css is missing the Markdown style %q", required)
		}
	}
}
