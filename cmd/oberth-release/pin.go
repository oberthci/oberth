package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/oberthci/oberth/internal/releaseverify"
)

const (
	releaseKeyPath  = "internal/releaseverify/release-cosign.pub"
	fetchScriptPath = "agent/skills/oberth/fetch-oberth"
	keyBlockStart   = "<<'OBERTH_RELEASE_PUBLIC_KEY'\n"
	keyBlockEnd     = "\nOBERTH_RELEASE_PUBLIC_KEY"
)

func releasePinCommand(o options) error {
	if o.publicKey == "" {
		return usage
	}
	return provisionReleaseKey(o.publicKey, o.replaceKey)
}

func readPublicKey(path string) ([]byte, error) {
	body, err := readKeyFile(path)
	if err != nil {
		return nil, err
	}
	key, err := releaseverify.CanonicalPublicKey(body)
	if err != nil {
		return nil, keyInvalid
	}
	return key, nil
}

func readKeyFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return readKeyBytes(file)
}

func readKeyBytes(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, releaseverify.MaxMetadata+1))
	if err != nil {
		return nil, err
	}
	if len(body) > releaseverify.MaxMetadata {
		return nil, keyInvalid
	}
	return body, nil
}

func checkKeyReplacement(current, candidate []byte, replace bool) error {
	if len(current) == 0 || replace {
		return nil
	}
	key, err := releaseverify.CanonicalPublicKey(current)
	if err != nil {
		return keyPinInvalid
	}
	if !bytes.Equal(key, candidate) {
		return keyReplace
	}
	return nil
}

func provisionReleaseKey(path string, replace bool) error {
	key, err := readPublicKey(path)
	if err != nil {
		return err
	}
	current, err := readKeyFile(releaseKeyPath)
	if err != nil {
		return err
	}
	if err = checkKeyReplacement(current, key, replace); err != nil {
		return err
	}
	script, err := os.ReadFile(fetchScriptPath)
	if err != nil {
		return err
	}
	updated, err := releaseKeySnapshot(string(script), key)
	if err != nil {
		return err
	}
	if err = writePinFile(releaseKeyPath, current, key); err != nil {
		return err
	}
	if err = writePinFile(fetchScriptPath, script, []byte(updated)); err != nil {
		return err
	}
	fmt.Printf("Canonical PEM SHA-256: %x\n", sha256.Sum256(key))
	return nil
}

func releaseKeySnapshot(script string, key []byte) (string, error) {
	if strings.Count(script, keyBlockStart) != 1 || strings.Count(script, keyBlockEnd) != 1 {
		return "", keyMarkers
	}
	before, rest, _ := strings.Cut(script, keyBlockStart)
	_, after, ok := strings.Cut(rest, keyBlockEnd)
	if !ok {
		return "", keyMarkers
	}
	body := strings.TrimSuffix(string(key), "\n")
	if len(key) == 0 {
		body = "RELEASE_KEY_PLACEHOLDER"
	}
	return before + keyBlockStart + body + keyBlockEnd + after, nil
}

func writePinFile(path string, before, after []byte) error {
	if bytes.Equal(before, after) {
		return nil
	}
	if err := os.WriteFile(path, after, 0644); err != nil {
		return err
	}
	fmt.Println("Updated", path)
	return nil
}
