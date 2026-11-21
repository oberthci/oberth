package runlog

import "testing"

func TestDefaultSelectionTrustsAuthoredRoleInsteadOfMainName(t *testing.T) {
	s := newStore(t, 1024)
	own := native("one")
	own.Container = "test"
	other := native("two")
	other.Main = false
	capture(t, s, own, "authored\n")
	capture(t, s, other, "executor named main\n")
	for _, request := range []ReadRequest{{OwnOnly: true}, {Selector: Selector{Step: "build"}}} {
		body, _, err := s.Read(t.Context(), "run", request)
		if err != nil || string(body) != "authored\n" {
			t.Fatal("name replaced caller-owned role", string(body), err)
		}
	}
}
