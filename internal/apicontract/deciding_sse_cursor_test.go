package apicontract_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestSSENULCursorCannotReplaceValidCursor(t *testing.T) {
	for _, tc := range []struct{ name, prefix, want string }{
		{"without prior cursor", "", ""},
		{"with prior cursor", "id: 17\n", "17"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			wire := tc.prefix + "id: unsafe\x00cursor\ndata: {}\n\n"
			err := apicontract.ReadSSE(strings.NewReader(wire), 128, func(frame apicontract.SSEFrame) error {
				count++
				if frame.ID != tc.want || string(frame.Data) != "{}" {
					t.Fatalf("invalid resume cursor delivered: %+v", frame)
				}
				return nil
			})
			if err != nil || count != 1 {
				t.Fatal("complete frame lost", count, err)
			}
		})
	}
}
