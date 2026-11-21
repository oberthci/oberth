package gitcache

import "testing"

func TestPreparationRecordsOnlyAdmittedNames(t *testing.T) {
	record := Reservation{Event: ReceiveEvent{Updates: []RefUpdate{{Ref: "refs/heads/admitted"}}}}
	updates := []RefUpdate{{Ref: "refs/heads/other"}, {Ref: "refs/heads/admitted"}}
	if !recordPreparation(&record, updates) {
		t.Fatal("preparation was not recorded")
	}
	if _, ok := record.Prepared["refs/heads/admitted"]; !ok || len(record.Prepared) != 1 {
		t.Fatal("preparation admitted an unrelated ref", record.Prepared)
	}
	if recordPreparation(&record, updates) {
		t.Fatal("repeated preparation changed durable facts")
	}
}
