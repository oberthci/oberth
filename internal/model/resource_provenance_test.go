package model

import "testing"

func TestResourceProvenanceMergeCannotChangeOwnerOrLimitsSilently(t *testing.T) {
	old := resourceModelRow()
	old.TemplateName = "frag-12345678-step-abc12345"
	old.FragmentRepository = "repo-01"
	n := int64(100)
	old.MemoryLimitBytes = &n
	next := old
	next.FragmentRepository = "repo-02"
	if _, e := MergeResourceUsage(old, next); e == nil {
		t.Fatal("owner changed")
	}
	next = old
	n2 := int64(200)
	next.MemoryLimitBytes = &n2
	got, e := MergeResourceUsage(old, next)
	if e != nil || !got.RequestsChanged || *got.MemoryLimitBytes != 100 {
		t.Fatalf("limits: %+v %v", got, e)
	}
}
