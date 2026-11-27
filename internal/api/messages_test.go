package api

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestTransportMessageCatalogue(t *testing.T) {
	for key, m := range messages() {
		if err := publicerr.CheckMessage(m.reason, 80); err != nil {
			t.Errorf("%s reason: %v", key, err)
		}
		if err := publicerr.CheckMessage(m.remedy, 120); err != nil {
			t.Errorf("%s remedy: %v", key, err)
		}
	}
}
