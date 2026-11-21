package sourcedelivery

import (
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/strictjson"
)

const (
	LoadPath                  = "/api/source/v1/load"
	ReceiptTrailer            = "Oberth-Source-Receipt"
	LoadContentType           = "application/gzip"
	MaxLoadRequestBytes       = 16 << 10
	MaxReceiptBytes           = 16 << 10
	MaxMainArchiveBytes int64 = 544 << 20
	MaxPinArchiveBytes  int64 = 160 << 20
)

type LoadRequest struct {
	Locator    string                    `json:"locator"`
	Descriptor sourcecontract.Descriptor `json:"descriptor"`
}
type Payload struct {
	Kind  sourcecontract.Kind `json:"kind"`
	Index *int                `json:"index"`
}
type StreamReceipt struct {
	AdmissionDigest string  `json:"admission_digest"`
	Payload         Payload `json:"payload"`
	Digest          string  `json:"digest"`
	Bytes           *int64  `json:"bytes"`
}
type LoadReceipt struct {
	Version    int                       `json:"version"`
	Locator    string                    `json:"locator"`
	Descriptor sourcecontract.Descriptor `json:"descriptor"`
	Stream     StreamReceipt             `json:"stream"`
}
type ErrorResponse struct {
	Version int    `json:"version"`
	Code    string `json:"code"`
	Limit   int64  `json:"limit,omitempty"`
	// Empty Unit denotes bytes; files denotes a source entry count.
	Unit    string `json:"unit,omitempty"`
	Message string `json:"message,omitempty"`
}

func DecodeRequest(data []byte) (LoadRequest, error) {
	var r LoadRequest
	if err := strictjson.DecodeStrictObject(data, &r, MaxLoadRequestBytes); err != nil {
		return r, err
	}
	if r.Locator == "" || len(r.Locator) > 128 {
		return r, errors.New(reasonRequest)
	}
	return r, r.Descriptor.Validate()
}
func DecodeDescriptor(data []byte) (sourcecontract.Descriptor, error) {
	var d sourcecontract.Descriptor
	if err := strictjson.DecodeStrictObject(data, &d, MaxLoadRequestBytes); err != nil {
		return d, err
	}
	return d, d.Validate()
}
func EncodeReceipt(r LoadReceipt) (string, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	if len(data) > MaxReceiptBytes {
		return "", errors.New(reasonReceipt)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
func VerifyReceipt(value string, request LoadRequest, digest string) error {
	if len(value) > base64.RawURLEncoding.EncodedLen(MaxReceiptBytes) {
		return errors.New(reasonReceipt)
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return errors.New(reasonReceipt)
	}
	var r LoadReceipt
	if strictjson.DecodeStrictObject(data, &r, MaxReceiptBytes) != nil {
		return errors.New(reasonReceipt)
	}
	if r.Version != 1 || r.Locator != request.Locator || r.Descriptor != request.Descriptor || r.Stream.Digest != digest {
		return errors.New(reasonReceipt)
	}
	if !validDigest(r.Stream.AdmissionDigest) || !validDigest(r.Stream.Digest) || r.Stream.Bytes == nil || *r.Stream.Bytes < 0 {
		return errors.New(reasonReceipt)
	}
	return validatePayload(r.Stream.Payload, request.Descriptor.Kind)
}
func validatePayload(p Payload, kind sourcecontract.Kind) error {
	if p.Kind != kind || p.Index == nil {
		return errors.New(reasonReceipt)
	}
	if *p.Index < 0 || *p.Index > 4 {
		return errors.New(reasonReceipt)
	}
	if p.Kind == sourcecontract.Main && *p.Index != 0 {
		return errors.New(reasonReceipt)
	}
	return nil
}

func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
