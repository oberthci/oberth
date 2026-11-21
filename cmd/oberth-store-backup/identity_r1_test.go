package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"testing"

	"filippo.io/age"
	"filippo.io/age/armor"
)

func TestIdentityOwnerModesAndExposure(t *testing.T) {
	for _, mode := range []os.FileMode{0400, 0600, 0640, 0644, 0604} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			c := cryptoFixture(t)
			must(t, os.Chmod(c.identity, mode))
			_, err := readIdentity(c.identity)
			if mode == 0400 || mode == 0600 {
				must(t, err)
				return
			}
			requireReason(t, err, identityPublic)
			if !bytes.Contains([]byte(err.Error()), []byte(fmt.Sprintf("%04o", mode))) {
				t.Fatal("refusal hides actual file mode", err)
			}
		})
	}
}

func TestIdentitySizeBoundIsIndependentOfImplementation(t *testing.T) {
	const boundary = 16 << 10
	for _, size := range []int{boundary, boundary + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			c := cryptoFixture(t)
			body, err := os.ReadFile(c.identity)
			must(t, err)
			body = append(body, '#')
			body = append(body, bytes.Repeat([]byte{'x'}, size-len(body))...)
			must(t, os.WriteFile(c.identity, body, 0600))
			_, err = readIdentity(c.identity)
			if size == boundary {
				must(t, err)
			} else {
				requireReason(t, err, bindingInvalid)
			}
		})
	}
}

func TestIdentityFormsExplainTheActualCause(t *testing.T) {
	for _, mode := range []string{"invalid", "several", "unsupported", "protected-binary", "protected-armored"} {
		t.Run(mode, func(t *testing.T) {
			c := cryptoFixture(t)
			body, err := os.ReadFile(c.identity)
			must(t, err)
			want := identityInvalid
			switch mode {
			case "invalid":
				body = []byte("not an identity")
			case "several":
				body = append(body, body...)
			case "unsupported":
				key, err := age.GenerateHybridIdentity()
				must(t, err)
				body = []byte(key.String() + "\n")
			default:
				body = protectIdentity(t, body, mode == "protected-armored")
				want = identityProtected
			}
			must(t, os.WriteFile(c.identity, body, 0600))
			_, err = readIdentity(c.identity)
			requireReason(t, err, want)
		})
	}
}

func protectIdentity(t *testing.T, body []byte, armored bool) []byte {
	t.Helper()
	recipient, err := age.NewScryptRecipient("fixture password")
	must(t, err)
	recipient.SetWorkFactor(1)
	var out bytes.Buffer
	var sink io.Writer = &out
	var wrapper io.WriteCloser
	if armored {
		wrapper = armor.NewWriter(&out)
		sink = wrapper
	}
	w, err := age.Encrypt(sink, recipient)
	must(t, err)
	_, err = w.Write(body)
	must(t, err)
	must(t, w.Close())
	if wrapper != nil {
		must(t, wrapper.Close())
	}
	return out.Bytes()
}
