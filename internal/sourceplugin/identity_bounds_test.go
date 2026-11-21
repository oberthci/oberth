package sourceplugin

import (
	"os"
	"strings"
	"testing"
)

func TestSelectedTokenFileAccepts1MiBRefusesOneMoreByte(t *testing.T) {
	path := identity(t)
	client := &Client{tokenFile: path}
	for _, size := range []int{1 << 20, (1 << 20) + 1} {
		if err := os.WriteFile(path, []byte("x"+strings.Repeat(" ", size-1)), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := client.bearer()
		if (err == nil) != (size == 1<<20) {
			t.Fatalf("token file %d: %v", size, err)
		}
		if err == nil && got != "x" {
			t.Fatal("selected token changed")
		}
	}
}
