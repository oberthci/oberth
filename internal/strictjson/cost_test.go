package strictjson

import (
	"bytes"
	"encoding/json"
	"runtime"
	"testing"
)

func TestOneMiBDecodeMemoryTracksStandardDecoder(t *testing.T) {
	const limit = 1 << 20
	data := append([]byte(`{"values":[0`), bytes.Repeat([]byte(",0"), (limit-16)/2)...)
	data = append(data, ']', '}')
	data = append(data, bytes.Repeat([]byte(" "), limit-len(data))...)
	standard := allocatedBytes(t, func() error {
		var value struct {
			Values []int `json:"values"`
		}
		return json.NewDecoder(bytes.NewReader(data)).Decode(&value)
	})
	bounded := allocatedBytes(t, func() error {
		var value struct {
			Values []int `json:"values"`
		}
		return DecodeStrictObject(data, &value, limit)
	})
	t.Logf("1 MiB standard: %d allocated bytes; bounded: %d allocated bytes", standard, bounded)
	if bounded > 2*standard+(64<<10) {
		t.Fatal("decode memory exceeds twice the standard decoder plus 64 KiB")
	}
}
func allocatedBytes(t *testing.T, decode func() error) uint64 {
	t.Helper()
	if err := decode(); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := decode(); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}
