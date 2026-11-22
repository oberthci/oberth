package gitssh

import "testing"

// Git hands the shim the user and path of the forge address; the shim logs in
// as that user, not as git.
func TestShimLogsInAsTheForgeAddressUser(t *testing.T) {
	c, err := Parse([]string{"-p", "2222", "oberth@192.168.86.247", "git-upload-pack '/home/oberth/repos/oberth-smoke.git'"})
	if err != nil || c.User != "oberth" || c.Host != "192.168.86.247" || c.Port != "2222" {
		t.Fatal(c, err)
	}
	plain, err := Parse([]string{"192.168.86.247", "git-upload-pack '/team/widget.git'"})
	if err != nil || plain.User != "" {
		t.Fatal("an address without a user named one", plain, err)
	}
}
