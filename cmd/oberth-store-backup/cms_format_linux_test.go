//go:build linux

package main

import (
	"bytes"
	"encoding/asn1"
	"testing"
)

func derValue(t *testing.T, tag byte, body ...[]byte) []byte {
	t.Helper()
	v, err := asn1.Marshal(asn1.RawValue{Class: int(tag >> 6), Tag: int(tag & 31), IsCompound: tag&32 != 0, Bytes: bytes.Join(body, nil)})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func derOID(t *testing.T, oid asn1.ObjectIdentifier) []byte {
	t.Helper()
	v, err := asn1.Marshal(oid)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAuthenticatedCMSClosedFormat(t *testing.T) {
	for _, mutation := range []string{"valid", "cbc-content", "wrong-cipher", "nonce", "mac", "icv", "attributes", "second-recipient", "trailing", "truncated", "indefinite"} {
		t.Run(mutation, func(t *testing.T) {
			contentOID := asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 23}
			cipherOID := asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 46}
			nonce, mac, icv := 12, 16, byte(16)
			if mutation == "cbc-content" {
				contentOID = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 3}
			}
			if mutation == "wrong-cipher" {
				cipherOID = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
			}
			if mutation == "nonce" {
				nonce = 11
			}
			if mutation == "mac" {
				mac = 15
			}
			if mutation == "icv" {
				icv = 12
			}
			recipient := derValue(t, 0x30, derValue(t, 2, []byte{0}))
			recipients := recipient
			if mutation == "second-recipient" {
				recipients = append(append([]byte(nil), recipient...), recipient...)
			}
			algorithm := derValue(t, 0x30, derOID(t, cipherOID), derValue(t, 0x30, derValue(t, 4, make([]byte, nonce)), derValue(t, 2, []byte{icv})))
			encrypted := derValue(t, 0x30, derOID(t, asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}), algorithm, derValue(t, 0x80, make([]byte, 256)))
			body := bytes.Join([][]byte{derValue(t, 2, []byte{0}), derValue(t, 0x31, recipients), encrypted, derValue(t, 4, make([]byte, mac))}, nil)
			if mutation == "attributes" {
				body = append(body, derValue(t, 0xa2, []byte{0})...)
			}
			encoded := derValue(t, 0x30, derOID(t, contentOID), derValue(t, 0xa0, derValue(t, 0x30, body)))
			if mutation == "trailing" {
				encoded = append(encoded, 0)
			}
			if mutation == "truncated" {
				encoded = encoded[:len(encoded)-1]
			}
			if mutation == "indefinite" {
				encoded[1] = 0x80
			}
			err := checkAuthenticatedCMS(derCursor{bytes.NewReader(encoded), 0, int64(len(encoded))})
			if (err == nil) != (mutation == "valid") {
				t.Fatalf("unexpected format verdict: %v", err)
			}
		})
	}
}
