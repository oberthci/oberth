package argojob

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestLiveVMIUnreadableCountRetainsReadableDimensions(t *testing.T) {
	for _, value := range []any{"64", 15.5, true, int64(0), int64(-1), json.Number("bad"), json.Number("15.5")} {
		object := liveVMIFixture(t, "hotplug-defaulted")
		setLiveVMIField(t, object, "cpu.cores", value)
		setLiveVMIField(t, object, "cpu.sockets", int64(16))
		setLiveVMIField(t, object, "memory.guest", "200Gi")
		demand, err := liveVMIDemand(object)
		if !errors.Is(err, errLiveVMIUnreadableCount) || demand.Cores < 16 || demand.MemoryBytes < 200<<30 {
			t.Fatalf("value %v lost unreadability or readable siblings: %+v %v", value, demand, err)
		}
	}
}
