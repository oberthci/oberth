package installer

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"slices"
	"strings"
)

type bundleReader struct {
	parts   *multipart.Reader
	members []bundleMember
}

func openInstallationBundle(in io.Reader) (*bundleReader, installationBackupInfo, error) {
	var info installationBackupInfo
	buffer := bufio.NewReader(in)
	line, err := buffer.ReadSlice('\n')
	boundary, found := strings.CutPrefix(strings.TrimSuffix(string(line), "\n"), installationBackupFormat+" ")
	if err != nil || !found || len(boundary) > 70 || boundary == "" {
		return nil, info, restoreProblem()
	}
	reader := &bundleReader{parts: multipart.NewReader(buffer, boundary)}
	if err = reader.json("installation.json", &info, 1<<20); err != nil || info.Version != 1 || info.Namespace == "" || info.ClusterUID == "" || (info.StoreIncluded && !info.BundledStore) {
		return nil, info, restoreProblem()
	}
	return reader, info, nil
}

func (r *bundleReader) read(name string, inspect func(io.Reader) error) error {
	part, err := r.parts.NextPart()
	if err != nil {
		return restoreProblem()
	}
	disposition, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || params["filename"] != name {
		return restoreProblem()
	}
	digest := sha256.New()
	count := &countingWriter{out: digest}
	in := io.TeeReader(part, count)
	if err = inspect(in); err != nil {
		return err
	}
	if _, err = io.Copy(io.Discard, in); err != nil || count.n == 0 {
		return restoreProblem()
	}
	r.members = append(r.members, bundleMember{Name: name, Bytes: count.n, SHA256: hex.EncodeToString(digest.Sum(nil))})
	return part.Close()
}

func (r *bundleReader) json(name string, out any, limit int64) error {
	return r.read(name, func(in io.Reader) error {
		raw, err := io.ReadAll(io.LimitReader(in, limit+1))
		defer clear(raw)
		if err != nil || int64(len(raw)) > limit || json.Unmarshal(raw, out) != nil {
			return restoreProblem()
		}
		return nil
	})
}

func decodeBackupSecrets(in io.Reader) ([]backupSecret, error) {
	raw, err := io.ReadAll(io.LimitReader(in, (8<<20)+1))
	defer clear(raw)
	if err != nil || len(raw) > 8<<20 {
		return nil, restoreProblem()
	}
	var secrets []backupSecret
	if json.Unmarshal(raw, &secrets) != nil || len(secrets) != len(installationSecretNames()) {
		clearBackupSecrets(secrets)
		return nil, restoreProblem()
	}
	seen := map[string]bool{}
	valid := true
	for _, secret := range secrets {
		valid = valid && !seen[secret.Name] && slices.Contains(installationSecretNames(), secret.Name) && len(secret.Data) > 0
		seen[secret.Name] = true
		for _, value := range secret.Data {
			valid = valid && len(value) > 0
		}
	}
	if !valid {
		clearBackupSecrets(secrets)
		return nil, restoreProblem()
	}
	return secrets, nil
}

func clearBackupSecrets(secrets []backupSecret) {
	for _, secret := range secrets {
		for _, value := range secret.Data {
			clear(value)
		}
	}
}
