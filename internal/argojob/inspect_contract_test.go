package argojob

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnedAssistLifecycleContract(t *testing.T) {
	_, path, _ := testHoldOwner(t)
	_, next := sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "status"})
	if f := finalHold(t, next); f.Type != "held" || f.TTL != 1200 {
		t.Fatal(f)
	}
	_, next = sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "exec", Argv: []string{"sh", "-c", "exit 7"}})
	if f := finalHold(t, next); f.Type != "exit" || f.Code != 7 {
		t.Fatal(f)
	}
	for range 2 {
		_, next = sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "release"})
		if f := finalHold(t, next); f.Type != "closed" {
			t.Fatal(f)
		}
	}
	_, next = sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "exec", Argv: []string{"true"}})
	if f := finalHold(t, next); f.Type != "closed" {
		t.Fatal("payload accepted after release", f)
	}
}
func TestBPublicHelperEntryInstall(t *testing.T) {
	to := t.TempDir()
	if code := RunHold(t.Context(), []string{"install", "--to", to}, HoldIO{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}); code != 0 {
		t.Fatal(code)
	}
	info, err := os.Stat(filepath.Join(to, "oberth-hold"))
	if err != nil || info.Mode().Perm() != 0555 || info.Size() == 0 {
		t.Fatal(info, err)
	}
}
