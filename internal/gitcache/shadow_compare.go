package gitcache

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os/exec"
	"strings"
)

// RunShadow is the isolated helper entry point. Only local immutable objects
// are read. go-git uses the normal verified storage, 32 MiB LRU and streaming
// decoder; archives are hashed in a fixed-size streaming buffer, never retained.
func RunShadow(ctx context.Context, repository, sha, gitBinary, globalConfig string, output io.Writer) error {
	if err := ValidateSHA(sha); err != nil {
		return err
	}
	cli := func(args []string, out io.Writer) error {
		// Inherit the helper's process group: the parent's timeout kills both
		// the helper and its Git child, including while a reader is blocked.
		cmd := exec.CommandContext(ctx, gitBinary, args...) //nolint:gosec
		cmd.Dir = repository
		cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp", "LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=" + globalConfig, "GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"}
		cmd.Stdout, cmd.Stderr = out, io.Discard
		return cmd.Run()
	}
	var tree shadowOutput
	if err := cli([]string{"rev-parse", "--verify", sha + "^{tree}"}, &tree); err != nil {
		return err
	}
	result := shadowComparison{SHA: sha, CLITree: strings.TrimSpace(tree.String())}
	var err error
	result.CLIArchive, err = shadowArchiveDigest(func(w io.Writer) error {
		return cli([]string{"archive", "--format=tar", sha}, w)
	})
	if err != nil {
		return err
	}
	r, closeStorage, err := openGoRepository(repository)
	if err != nil {
		return err
	}
	defer closeStorage()
	h, err := resolveGoObject(r, sha, true)
	if err != nil {
		return err
	}
	commit, err := r.CommitObject(h)
	if err != nil {
		return err
	}
	result.GoTree = commit.TreeHash.String()
	result.GoArchive, err = shadowArchiveDigest(func(w io.Writer) error { return goArchive(ctx, r, sha, w) })
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}

func shadowArchiveDigest(produce func(io.Writer) error) (string, error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := produce(writer)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	digest, err := hashShadowArchive(reader)
	_ = reader.CloseWithError(err)
	return digest, errors.Join(err, <-done)
}

// The comparison hashes every file byte, plus length-framed entry metadata.
// Git's global PAX comment/padding and Go's tar framing differ legitimately.
// Ignore only framing, not modes, paths, symlinks, ordering or file contents.
func hashShadowArchive(input io.Reader) (string, error) {
	digest := sha256.New()
	archive := tar.NewReader(input)
	buffer := make([]byte, 32<<10)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			// Drain the producer's trailer so it cannot block closing its pipe.
			_, err = io.CopyBuffer(io.Discard, input, buffer)
			return hex.EncodeToString(digest.Sum(nil)), err
		}
		if err != nil {
			return "", err
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		for _, field := range []string{header.Name, fmt.Sprint(header.Mode), fmt.Sprint(header.Typeflag), header.Linkname, fmt.Sprint(header.Size)} {
			hashShadowField(digest, field)
		}
		n, err := io.CopyBuffer(digest, io.LimitReader(archive, header.Size), buffer)
		if err != nil {
			return "", err
		}
		if n != header.Size {
			return "", io.ErrUnexpectedEOF
		}
	}
}

func hashShadowField(h hash.Hash, field string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(field)))
	_, _ = h.Write(length[:])
	_, _ = io.WriteString(h, field)
}

func validShadowDigest(value string) bool {
	body, err := hex.DecodeString(value)
	return err == nil && len(body) == sha256.Size
}
