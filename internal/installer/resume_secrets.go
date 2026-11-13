package installer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

const resumeSecretLimit = 1 << 20

func openResumeInput(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: explicit recovery path; validate the opened descriptor below.
	if err != nil {
		return nil, errors.New("cannot open install recovery input")
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, errors.New("cannot inspect install recovery input")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || int64(stat.Uid) != int64(os.Geteuid()) || stat.Nlink != 1 || info.Size() > resumeSecretLimit {
		_ = f.Close()
		return nil, errors.New("install recovery input must be an owner-only regular file owned by this user, without links, at most 1 MiB")
	}
	return f, nil
}

type resumeBuffer struct{ bytes.Buffer }

func (b *resumeBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > resumeSecretLimit {
		return 0, errors.New("recovery input exceeds limit")
	}
	return b.Buffer.Write(p)
}

func readResumeRootToken(ctx context.Context, path, identity string) ([]byte, error) {
	f, err := openResumeInput(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var raw []byte
	if identity != "" {
		key, err := openResumeInput(identity)
		if err != nil {
			return nil, err
		}
		defer func() { _ = key.Close() }()
		var plain resumeBuffer
		defer func() { clear(plain.Bytes()) }()
		command := exec.CommandContext(ctx, "age", "--decrypt", "--identity", "/dev/fd/3")
		command.Env = installerChildEnvironment()
		command.ExtraFiles = []*os.File{key}
		command.Stdin = f
		command.Stdout = &plain
		command.Stderr = io.Discard
		if err := command.Run(); err != nil {
			return nil, errors.New("cannot decrypt install recovery file; check age and the identity file")
		}
		raw = append([]byte(nil), plain.Bytes()...)
	} else {
		raw, err = io.ReadAll(io.LimitReader(f, resumeSecretLimit+1))
		if err != nil || len(raw) > resumeSecretLimit {
			return nil, errors.New("cannot read install recovery file")
		}
	}
	defer clear(raw)
	var token []byte
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte("OPENBAO_ROOT_TOKEN=")) {
			continue
		}
		value := bytes.TrimPrefix(line, []byte("OPENBAO_ROOT_TOKEN="))
		if token != nil || len(value) == 0 || len(value) > 4096 || strings.ContainsAny(string(value), " \t\r\x00") {
			clear(token)
			return nil, errors.New("install recovery file must contain exactly one valid root token")
		}
		token = append([]byte(nil), value...)
	}
	if token == nil {
		return nil, errors.New("install recovery file has no OPENBAO_ROOT_TOKEN; encrypted files require --resume-age-identity")
	}
	return token, nil
}
