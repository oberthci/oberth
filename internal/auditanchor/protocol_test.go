package auditanchor

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestConfiguredWitnessProtocolRetainsExactKey(t *testing.T) {
	material := []byte("generic persistent host key")
	key, err := deriveWitnessKeyWithInfo(material, "sample-oberth-audit-witness-v1")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := key.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	// Independently calculated RFC5869 SHA256 vector: 48-byte HKDF expansion,
	// reduce modulo P-256's order minus one, then add one as in the protocol.
	const want = "01d35d84882e3e46d723b925466db1bc4d95c06f6e8d07a3433f1c496ea099ce"
	if hex.EncodeToString(encoded) != want {
		t.Fatal("configured historical witness key changed")
	}
	fresh, err := deriveWitnessKey(material)
	if err != nil {
		t.Fatal(err)
	}
	freshBytes, err := fresh.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(encoded, freshBytes) {
		t.Fatal("different witness protocol domains share an identity")
	}
	for _, info := range []string{"bad\nvalue", "bad\x00value", "*"} {
		if _, err := deriveWitnessKeyWithInfo(material, info); err == nil {
			t.Fatal("unsafe witness info accepted")
		}
	}
}
