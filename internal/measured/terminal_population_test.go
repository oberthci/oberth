package measured

import "testing"

func TestSuggestionsExcludeRunningAttempts(t *testing.T) {
	rows := observations()
	baseline := Build(rows, nil)[0]
	for i := 0; i < 200; i++ {
		running := rows[0]
		running.Usage.RunID = "running-" + string(rune(i+1000))
		running.Usage.PodUID = running.Usage.RunID
		running.Usage.Terminal = false
		running.Usage.MemoryPeakBytes = value(1)
		running.Usage.CPUTimeNS = value(1)
		rows = append(rows, running)
	}
	got := Build(rows, nil)[0]
	if got.Suggested == nil || *got.Suggested != *baseline.Suggested || got.Attempts != baseline.Attempts || *got.MemoryPeakBytes.P95 != *baseline.MemoryPeakBytes.P95 {
		t.Fatalf("running attempts changed advice: got %+v; want %+v", got.Suggested, baseline.Suggested)
	}
}
