package apicontract_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestSSEIgnoresLegacyCursorFields(t *testing.T) {
	for _, tc := range []struct{ name, prefix string }{
		{"without prior cursor", ""},
		{"with prior cursor", "id: 17\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			wire := tc.prefix + "id: unsafe\x00cursor\ndata: {}\n\n"
			err := apicontract.ReadSSE(strings.NewReader(wire), 128, func(frame apicontract.SSEFrame) error {
				count++
				if string(frame.Data) != "{}" {
					t.Fatalf("legacy cursor changed request data: %+v", frame)
				}
				return nil
			})
			if err != nil || count != 1 {
				t.Fatal("complete frame lost", count, err)
			}
		})
	}
}
