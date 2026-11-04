package model

import (
	"testing"
	"time"
)

func TestAdmissionHoldClassesAndExpiry(t *testing.T) {
	now := time.Now().UTC()
	runs := []Run{{RefKind: RefBranch}, {RefKind: RefTag}, {RefKind: RefBranch, Release: true}, {RefKind: RefBranch, Trigger: "promotion"}, {RefKind: RefTag, Release: true, Trigger: "promotion"}}
	for _, test := range []struct {
		name    string
		allow   []string
		allowed []bool
	}{
		{"default", nil, []bool{false, true, true, false, true}},
		{"none", []string{}, []bool{false, false, false, false, false}},
		{"tag", []string{"tag"}, []bool{false, true, false, false, true}},
		{"release", []string{"release"}, []bool{false, false, true, false, true}},
		{"promotion", []string{"promotion"}, []bool{false, false, false, true, true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := (AdmissionHoldRequest{On: true, Reason: "qualification", Allow: test.allow}).Normalize()
			if err != nil || request.Duration != 2*time.Hour {
				t.Fatal(request, err)
			}
			hold := AdmissionHold{On: true, Allow: request.Allow, ExpiresAt: now.Add(request.Duration)}
			for i, run := range runs {
				if hold.Blocks(run, now) == test.allowed[i] {
					t.Fatalf("class %d allowed=%v", i, test.allowed[i])
				}
				if hold.Blocks(run, hold.ExpiresAt) || hold.Snapshot(hold.ExpiresAt).On {
					t.Fatal("hold survives exact expiry")
				}
			}
			if len(hold.Allow) > 0 {
				snapshot := hold.Snapshot(now)
				snapshot.Allow[0] = "changed"
				if hold.Allow[0] == "changed" {
					t.Fatal("snapshot aliases cached hold")
				}
			}
		})
	}
	for _, duration := range []time.Duration{time.Nanosecond, 12 * time.Hour} {
		if _, err := (AdmissionHoldRequest{On: true, Reason: "bounded", Duration: duration}).Normalize(); err != nil {
			t.Fatal(err)
		}
	}
	for _, request := range []AdmissionHoldRequest{
		{On: true, Reason: "\n"}, {On: true, Reason: "two\nlines"}, {On: true, Reason: "x", Duration: 12*time.Hour + time.Nanosecond},
		{Reason: "off", Allow: []string{}},
	} {
		if _, err := request.Normalize(); err == nil {
			t.Fatal("invalid request accepted", request)
		}
	}
}

func TestAdmissionHoldShortMessageKeepsDetailVocabularySeparate(t *testing.T) {
	hold := AdmissionHold{Reason: "release qualification", ExpiresAt: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	if got := hold.ShortMessage(); got != "queue held by admin: release qualification, until 2026-11-04T12:00:00Z" {
		t.Fatal(got)
	}
	if got := hold.Message(); got != "admission held by admin: release qualification, until 2026-11-04T12:00:00Z" {
		t.Fatal(got)
	}
}
