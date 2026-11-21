package installer_test

import "testing"

func TestChartCarriesOneReceivePackLimit(t *testing.T) {
	for _, limit := range []int64{1 << 30, 2048} {
		var values map[string]any
		if limit != 1<<30 {
			values = map[string]any{"server": map[string]any{"runtime": map[string]any{"receive_max_input_bytes": limit}}}
		}
		cfg, err := decodeChartServer([]byte(jsonOf(t, serverJSON(t, rendered(t, values)))))
		if err != nil || cfg.Runtime.ReceiveMaxInputBytes != limit {
			t.Fatal("chart lost receive pack limit", cfg.Runtime.ReceiveMaxInputBytes, limit, err)
		}
	}
	for _, limit := range []int64{0, -1} {
		if err := compositionRenderError(t, map[string]any{"server": map[string]any{"runtime": map[string]any{"receive_max_input_bytes": limit}}}); err == nil {
			t.Fatal("unbounded receive setting rendered", limit)
		}
	}
}
