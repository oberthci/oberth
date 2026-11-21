package releaseverify

import (
	"errors"
	"io/fs"
	"testing"
)

type neverReadReleaseFS struct{ t *testing.T }

func (s neverReadReleaseFS) Open(string) (fs.File, error) {
	s.t.Fatal("empty embedded pin opened untrusted input")
	return nil, fs.ErrNotExist
}

func TestReviewEmptyEmbeddedPinRefuses(t *testing.T) {
	if len(pinnedKey) != 0 {
		t.Skip("owner signing pin is provisioned")
	}
	for _, version := range []string{"v0.1.0", "", "../../outside", "v999.0.0"} {
		if _, err := Verify(t.Context(), neverReadReleaseFS{t}, version); !errors.Is(err, Key) {
			t.Fatal("missing pin did not refuse")
		}
	}
}
