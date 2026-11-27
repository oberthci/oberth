package installer

import "testing"

func TestResourceChoicesNeverExposeNativeTemplateInputs(t *testing.T) {
	for _, name := range []string{"argoNative", "storeNative"} {
		if allowedOverrides(map[string]any{name: map[string]any{"controller": map[string]any{"extraArgs": []any{"--other"}}}}) {
			t.Fatal("native command inputs became public choices", name)
		}
	}
}
