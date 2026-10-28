package installer

// Which key opens a backup (#072). An archive names its recipient in the
// clear (CMS RecipientInfo: issuer and serial of the certificate it was
// encrypted to), so a restore can say which key file it needs, and can test
// a candidate key against the archive's wrapped content key before running
// OpenSSL.

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
)

// BackupRecipient identifies the certificate an archive is encrypted to:
// by issuer and serial, or by subject key identifier.
type BackupRecipient struct {
	Issuer       string
	Serial       string
	SubjectKeyID string
}

func (r BackupRecipient) String() string {
	if r.SubjectKeyID != "" {
		return "subject key identifier " + r.SubjectKeyID
	}
	return r.Issuer + ", serial " + r.Serial
}

// archiveRecipient is one key-transport RecipientInfo of an archive.
type archiveRecipient struct {
	ID BackupRecipient
	// oaepSHA256 is true when the content key is wrapped with RSA-OAEP over
	// SHA-256, the only transport a candidate key can be tested against here.
	oaepSHA256   bool
	encryptedKey []byte
}

// recipientHeaderBytes bounds how much of an archive is read to find its
// recipients: they precede the encrypted content.
const recipientHeaderBytes = 64 << 10

var (
	oidRSAESOAEP = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 7}
	oidSHA256    = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
)

// readTLV reads one DER tag-length header at data and returns the tag byte,
// the content and the remainder. Only definite lengths occur in archives.
func readTLV(data []byte) (tag byte, content, rest []byte, err error) {
	if len(data) < 2 {
		return 0, nil, nil, errors.New("truncated")
	}
	tag, length, offset := data[0], int(data[1]), 2
	if length&0x80 != 0 {
		n := length & 0x7f
		if n == 0 || n > 5 || len(data) < 2+n {
			return 0, nil, nil, errors.New("unsupported length")
		}
		length = 0
		for _, b := range data[2 : 2+n] {
			length = length<<8 | int(b)
		}
		offset += n
	}
	if length < 0 || int64(length) > maxBackupCipherBytes {
		return 0, nil, nil, errors.New("implausible length")
	}
	if len(data)-offset < length {
		// The content runs past what was read; return what there is.
		return tag, data[offset:], nil, nil
	}
	return tag, data[offset : offset+length], data[offset+length:], nil
}

// archiveRecipients reads the RecipientInfos of a CMS AuthEnvelopedData
// archive without decrypting anything.
func archiveRecipients(path string) ([]archiveRecipient, error) {
	file, err := os.Open(path) // #nosec G304 -- explicit operator archive path.
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	header, err := io.ReadAll(io.LimitReader(file, recipientHeaderBytes))
	if err != nil {
		return nil, err
	}
	malformed := errors.New("backup archive header is not a CMS AuthEnvelopedData")
	// ContentInfo SEQUENCE { contentType OID, [0] AuthEnvelopedData }
	tag, info, _, err := readTLV(header)
	if err != nil || tag != 0x30 {
		return nil, malformed
	}
	contentType := mustMarshalOID(authEnvelopedDataOID)
	if !bytes.HasPrefix(info, contentType) {
		return nil, malformed
	}
	tag, explicit, _, err := readTLV(info[len(contentType):])
	if err != nil || tag != 0xa0 {
		return nil, malformed
	}
	tag, envelope, _, err := readTLV(explicit)
	if err != nil || tag != 0x30 {
		return nil, malformed
	}
	// AuthEnvelopedData SEQUENCE { version, originatorInfo [0] OPTIONAL, recipientInfos SET, ... }
	if tag, _, envelope, err = readTLV(envelope); err != nil || tag != 0x02 {
		return nil, malformed
	}
	tag, set, _, err := readTLV(envelope)
	if err == nil && tag == 0xa0 {
		_, _, envelope, _ = readTLV(envelope)
		tag, set, _, err = readTLV(envelope)
	}
	if err != nil || tag != 0x31 {
		return nil, malformed
	}
	var recipients []archiveRecipient
	for len(set) > 0 {
		var info []byte
		tag, info, set, err = readTLV(set)
		if err != nil {
			return nil, malformed
		}
		if tag != 0x30 {
			continue // kari, kekri, pwri, ori: not a key-transport recipient
		}
		var ktri struct {
			Version      int
			RID          asn1.RawValue
			Algorithm    pkix.AlgorithmIdentifier
			EncryptedKey []byte
		}
		full, err := asn1.Marshal(asn1.RawValue{Tag: asn1.TagSequence, IsCompound: true, Bytes: info})
		if err != nil {
			continue
		}
		if _, err := asn1.Unmarshal(full, &ktri); err != nil {
			continue
		}
		recipient := archiveRecipient{encryptedKey: ktri.EncryptedKey}
		if ktri.RID.Class == asn1.ClassUniversal && ktri.RID.Tag == asn1.TagSequence {
			var issuerSerial struct {
				Issuer asn1.RawValue
				Serial *big.Int
			}
			if _, err := asn1.Unmarshal(ktri.RID.FullBytes, &issuerSerial); err == nil {
				var name pkix.RDNSequence
				if _, err := asn1.Unmarshal(issuerSerial.Issuer.FullBytes, &name); err == nil {
					var issuer pkix.Name
					issuer.FillFromRDNSequence(&name)
					recipient.ID = BackupRecipient{Issuer: issuer.String(), Serial: strings.ToUpper(issuerSerial.Serial.Text(16))}
				}
			}
		} else {
			recipient.ID = BackupRecipient{SubjectKeyID: fmt.Sprintf("%X", ktri.RID.Bytes)}
		}
		recipient.oaepSHA256 = ktri.Algorithm.Algorithm.Equal(oidRSAESOAEP) && bytes.Contains(ktri.Algorithm.Parameters.FullBytes, mustMarshalOID(oidSHA256))
		recipients = append(recipients, recipient)
	}
	if len(recipients) == 0 {
		return nil, errors.New("backup archive names no key-transport recipient")
	}
	return recipients, nil
}

func mustMarshalOID(oid asn1.ObjectIdentifier) []byte {
	encoded, err := asn1.Marshal(oid)
	if err != nil {
		panic(err)
	}
	return encoded
}

// ArchiveRecipients names the certificates an archive is encrypted to.
func ArchiveRecipients(path string) ([]BackupRecipient, error) {
	recipients, err := archiveRecipients(path)
	if err != nil {
		return nil, err
	}
	ids := make([]BackupRecipient, 0, len(recipients))
	for _, recipient := range recipients {
		ids = append(ids, recipient.ID)
	}
	return ids, nil
}

// errKeyUncheckable means a candidate key could not be tested here (an
// encrypted or non-RSA key, or a transport other than RSA-OAEP-SHA256);
// OpenSSL may still open the archive with it.
var errKeyUncheckable = errors.New("key cannot be checked against the archive")

// BackupKeyOpens reports whether the private key at keyPath unwraps the
// archive's content key. It reads the key, never prints it, and discards
// the unwrapped content key at once.
func BackupKeyOpens(archive, keyPath string) (bool, error) {
	recipients, err := archiveRecipients(archive)
	if err != nil {
		return false, err
	}
	key, err := readRSAPrivateKey(keyPath)
	if err != nil {
		return false, err
	}
	checked := false
	for _, recipient := range recipients {
		if !recipient.oaepSHA256 {
			continue
		}
		checked = true
		contentKey, err := rsa.DecryptOAEP(sha256.New(), nil, key, recipient.encryptedKey, nil)
		if err == nil {
			clear(contentKey)
			return true, nil
		}
	}
	if !checked {
		return false, errKeyUncheckable
	}
	return false, nil
}

// IsUncheckableKey reports whether err only means the key could not be
// tested before decryption.
func IsUncheckableKey(err error) bool { return errors.Is(err, errKeyUncheckable) }

func readRSAPrivateKey(path string) (*rsa.PrivateKey, error) {
	if err := validateBackupPrivateKey(path); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- explicit operator key path, validated above.
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%w: %s holds no PEM block", errKeyUncheckable, path)
	}
	defer clear(block.Bytes)
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errKeyUncheckable, err)
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%w: not an RSA key", errKeyUncheckable)
		}
		return key, nil
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errKeyUncheckable, err)
		}
		return key, nil
	}
	return nil, fmt.Errorf("%w: %s is a %s", errKeyUncheckable, path, block.Type)
}

// BackupRecipientOf names the certificate at path as an archive names its
// recipient: issuer and serial.
func BackupRecipientOf(certPath string) (BackupRecipient, error) {
	raw, err := os.ReadFile(certPath) // #nosec G304 -- explicit operator path to a public certificate.
	if err != nil {
		return BackupRecipient{}, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return BackupRecipient{}, fmt.Errorf("%s holds no PEM certificate", certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return BackupRecipient{}, err
	}
	return BackupRecipient{Issuer: cert.Issuer.String(), Serial: strings.ToUpper(cert.SerialNumber.Text(16))}, nil
}
