package goproxy

import "testing"

func cachedTestHandler(t *testing.T, cache GitCache, prefix string, routes map[string]string) *Handler {
	t.Helper()
	h, err := NewHandler(cache, prefix, routes).WithArtifactCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func mockModuleCache(tags []string, module string) *mockCache {
	c := &mockCache{tags: tags, files: map[string][]byte{}}
	for _, tag := range tags {
		c.files[tag+":go.mod"] = []byte("module " + module + "\n\ngo 1.22\n")
	}
	return c
}
