package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/releaseverify"
)

func TestPublicationCredentialByteLimit(t *testing.T) {
	for _, size := range []int{0, 1 << 20, 1<<20 + 1} {
		path := filepath.Join(t.TempDir(), "credential")
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0600); err != nil {
			t.Fatal(err)
		}
		p, err := publicationFor(options{token: path, dir: t.TempDir(), origin: releaseverify.Origin})
		valid := size == 1<<20
		if (err == nil) != valid || valid && len(p.token) != size {
			t.Fatal("credential was empty, unbounded or truncated", size, err)
		}
	}
}

func TestPublicationRefusesFIFOWithoutReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	keeper, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer keeper.Close()
	done := make(chan error, 1)
	dir := t.TempDir()
	go func() {
		_, err := publicationFor(options{token: path, dir: dir, origin: releaseverify.Origin})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, releaseToken) {
			t.Fatal("FIFO was not refused as a nonregular credential", err)
		}
	case <-time.After(2 * time.Second):
		_ = keeper.Close()
		<-done
		t.Fatal("publication read a FIFO before checking its type")
	}
}

func TestPublicationChecksTheOpenedCredentialAfterPathReplacement(t *testing.T) {
	for _, mode := range []os.FileMode{0600, 0644} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credential")
			if err := os.WriteFile(path, []byte("original"), mode); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := os.Rename(path, path+".moved"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err = readPublicationCredential(file, t.TempDir())
			want := error(releaseToken)
			if mode == 0644 {
				want = tokenShared
			}
			if !errors.Is(err, want) {
				t.Fatal("credential checks judged a replacement path", err, want)
			}
		})
	}
}
