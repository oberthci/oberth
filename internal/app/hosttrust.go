package app

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostScanner offers one public key per algorithm through a context-bound SSH handshake.
type HostScanner interface {
	Scan(context.Context, string, string) (ssh.PublicKey, error)
}
type TrustRequest struct {
	Address      string
	Fingerprints []string
}
type TrustResult struct {
	Lines             []byte
	Offered, Replaced []string
}

func TrustHost(ctx context.Context, scanner HostScanner, r TrustRequest) (TrustResult, error) {
	result := TrustResult{}
	if !hostAddress(r.Address) {
		return result, refusal("trust", nil)
	}
	var failures []string
	for _, algorithm := range []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA} {
		key, err := scanner.Scan(ctx, r.Address, algorithm)
		if err != nil {
			if ctx.Err() != nil {
				return result, refusal("trust", err)
			}
			if !slices.Contains(failures, err.Error()) {
				failures = append(failures, err.Error())
			}
			continue
		}
		fp := ssh.FingerprintSHA256(key)
		if slices.Contains(result.Offered, fp) {
			continue
		}
		result.Offered = append(result.Offered, fp)
		if slices.Contains(r.Fingerprints, fp) {
			result.Lines = append(result.Lines, []byte(knownhosts.Line([]string{r.Address}, key)+"\n")...)
		}
	}
	if len(result.Lines) == 0 {
		// Without an offered key, the scan failures are the only account of what happened.
		var cause error
		if len(result.Offered) == 0 && len(failures) > 0 {
			cause = errors.New(strings.Join(failures, "; "))
		}
		return result, refusal("trust", cause)
	}
	return result, nil
}

// Repin preserves unrelated bytes; the identity owner persists and audits replacement.
func Repin(ctx context.Context, scanner HostScanner, r TrustRequest, old []byte) (TrustResult, error) {
	kept, replaced, err := removeAddress(r.Address, old)
	if err != nil {
		return TrustResult{}, err
	}
	result, err := TrustHost(ctx, scanner, r)
	result.Replaced = replaced
	if err != nil {
		return result, err
	}
	if len(kept) > 0 && kept[len(kept)-1] != '\n' {
		result.Lines = append(result.Lines, kept...)
	} else {
		result.Lines = append(kept, result.Lines...)
	}
	return result, nil
}
func removeAddress(address string, old []byte) ([]byte, []string, error) {
	var kept []byte
	var replaced []string
	normalized := knownhosts.Normalize(address)
	for _, line := range strings.SplitAfter(string(old), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			kept = append(kept, line...)
			continue
		}
		marker, hosts, key, _, _, err := ssh.ParseKnownHosts([]byte(line))
		if err != nil {
			return nil, nil, refusal("pins", err)
		}
		for _, host := range hosts {
			if strings.ContainsAny(host, "*?!|") {
				return nil, nil, refusal("pins", nil)
			}
		}
		if slices.Contains(hosts, normalized) {
			if len(hosts) != 1 || marker != "" {
				return nil, nil, refusal("pins", nil)
			}
			replaced = append(replaced, ssh.FingerprintSHA256(key))
			continue
		}
		kept = append(kept, line...)
	}
	return kept, replaced, nil
}

func hostAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	return err == nil && host != "" && port != "" && !strings.ContainsAny(address, "\r\n\x00 ,/\t*?!|#\\")
}
