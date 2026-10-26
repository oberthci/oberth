package containerimage

import "testing"

func TestServerExecContract(t *testing.T) {
	for _, name := range []string{"oberth", "/usr/local/bin/oberth"} {
		command := ServerCommand([]string{name, "version"})
		if command[0] != "/usr/local/bin/oberth" {
			t.Fatal(command)
		}
	}
	for _, command := range [][]string{nil, {"cat", "/data/oberth.sqlite"}, {"sh", "-c", "true"}, {"/bin/sh", "-c", "true"}} {
		t.Run("reject", func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("non-native server exec accepted")
				}
			}()
			ServerCommand(command)
		})
	}
}
