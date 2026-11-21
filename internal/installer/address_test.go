package installer

import (
	"errors"
	"net"
	"net/url"
	"testing"
)

func TestAddressValidationCoversExplicitAndDerivedZeros(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "0", "0.0", "0.0.0", "0x0", "0X000", "00.00.00.00", "0x0.00.0.0", "0.", "::", "0:0:0:0:0:0:0:0", "::ffff:0.0.0.0", "::ffff:0:0", "0000:0000:0000:0000:0000:ffff:0000:0000", "::%eth0"} {
		for _, explicit := range []bool{false, true} {
			c := Choices{Context: "chosen"}
			if explicit {
				c.URL = (&url.URL{Scheme: "https", Host: net.JoinHostPort(host, "30443")}).String()
			}
			_, err := resolve(c, host)
			var refusal *Problem
			if !errors.As(err, &refusal) || refusal.Code != "address" {
				t.Fatal("zero host accepted or wrong refusal", host, explicit, err)
			}
			if refusal.Remedy != "Run oberth install with --url and --ssh-port clients can reach." {
				t.Fatal(refusal.Remedy)
			}
		}
	}
}

func TestAddressValidationKeepsReachableAndRejectsMalformedOrigins(t *testing.T) {
	for _, address := range []string{"https://127.0.0.1:30443", "https://[::1]:30443", "https://localhost.", "https://0.example", "https://0x0.example", "https://ci.example/", "https://[fe80::1%25eth0]:30443"} {
		if err := validateURL(address); err != nil {
			t.Fatal(address, err)
		}
	}
	for _, address := range []string{"http://ci.example", "https://", "https://user@ci.example", "https://ci.example/path", "https://ci.example?secret=x", "https://ci.example?", "https://ci.example#", "https://ci.example#fragment", "https://ci.example:0", "https://ci.example:65536"} {
		var refusal *Problem
		if err := validateURL(address); !errors.As(err, &refusal) || refusal.Code != "url" {
			t.Fatal("malformed origin accepted", address, err)
		}
	}
}
