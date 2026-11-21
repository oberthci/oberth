package apicontract_test

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"testing"
)

func TestCanonicalSecretDigest(t *testing.T) {
	if got := apicontract.SecretDigest("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal("wire digest changed")
	}
}
