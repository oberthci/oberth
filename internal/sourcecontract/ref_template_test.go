package sourcecontract

import "testing"

func TestPublicRefsKeepLiteralTemplateSyntax(t *testing.T) {
	for _, ref := range []string{"refs/heads/{{workflow.name}}", "refs/heads/a{{literal", "refs/tags/{{workflow.uid}}", "refs/heads/literal}}name"} {
		if err := ValidatePublicRef(ref); err != nil {
			t.Fatal("literal public ref refused", ref, err)
		}
	}
}
