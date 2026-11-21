package sshserver

import "testing"

func TestReviewSecondSingleQuoteCommand(t *testing.T) {
	for _, s := range []string{"git-upload-pack '", "git-receive-pack '"} {
		t.Run(s, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("authenticated exec panics instead of refusing: %v", p)
				}
			}()
			if _, e := ParseCommand(s); e == nil {
				t.Error("invalid command accepted")
			}
		})
	}
}
