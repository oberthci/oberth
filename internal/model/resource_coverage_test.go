package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestResourceMetricCoverageRoundTripAndDistinctSources(t *testing.T) {
	v := resourceModelRow()
	var doc map[string]any
	raw, _ := json.Marshal(v)
	_ = json.Unmarshal(raw, &doc)
	at := v.LastObservedAt
	doc["storage_coverage"] = map[string]any{"count": 1, "first_at": at, "last_at": at}
	raw, _ = json.Marshal(doc)
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		next := v
		next.LastObservedAt = at.Add(time.Duration(i) * time.Second)
		doc["last_observed_at"] = next.LastObservedAt
		doc["source_at"] = next.LastObservedAt
		// Only CPU refreshes; the storage timestamp remains identical.
		raw, _ = json.Marshal(doc)
		_ = json.Unmarshal(raw, &next)
		var err error
		v, err = MergeResourceUsage(v, next)
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, _ = json.Marshal(v)
	var got map[string]json.RawMessage
	_ = json.Unmarshal(raw, &got)
	var coverage struct {
		Count int64 `json:"count"`
	}
	if err := json.Unmarshal(got["storage_coverage"], &coverage); err != nil || coverage.Count != 1 {
		t.Fatalf("coverage missing or unrelated polls inflated it: %s", raw)
	}
}

func TestResourceMetricCoverageRejectsInvalidTimes(t *testing.T) {
	for _, kind := range []string{"negative", "missing", "future", "reversed"} {
		v := resourceModelRow()
		raw, _ := json.Marshal(v)
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		coverage := map[string]any{"count": 1, "first_at": v.LastObservedAt, "last_at": v.LastObservedAt}
		switch kind {
		case "negative":
			coverage["count"] = -1
		case "missing":
			delete(coverage, "first_at")
		case "future":
			coverage["last_at"] = v.LastObservedAt.Add(time.Hour)
		case "reversed":
			coverage["first_at"] = v.LastObservedAt.Add(time.Second)
		}
		doc["storage_coverage"] = coverage
		raw, _ = json.Marshal(doc)
		_ = json.Unmarshal(raw, &v)
		if v.Validate() == nil {
			t.Fatalf("invalid %s coverage accepted", kind)
		}
	}
}

func TestMetricCoverageDistinctSamplesAndConservativeOverlap(t *testing.T) {
	at := time.Now().UTC()
	var c MetricCoverage
	for i := range 4 {
		p := MetricCoverage{Count: 1, FirstAt: at.Add(time.Duration(i) * time.Second), LastAt: at.Add(time.Duration(i) * time.Second)}
		c = c.Merge(p)
		if got := c.Merge(p); got != c {
			t.Fatal("duplicate inflated coverage")
		}
	}
	if c.Count != 4 || !c.Through(at.Add(3*time.Second), 4) {
		t.Fatalf("distinct metric samples lost: %+v", c)
	}
	if got := c.Merge(MetricCoverage{Count: 3, FirstAt: at.Add(time.Second), LastAt: at.Add(4 * time.Second)}); got.Count != 4 {
		t.Fatal("overlapping aggregate invented samples", got)
	}
}
