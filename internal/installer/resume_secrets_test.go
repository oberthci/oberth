package installer

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResumeInstallReadsPrivateRootWithoutOutput(t *testing.T) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(random)
	path := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(path, []byte("OPENBAO_ROOT_TOKEN="+token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cfg := Config{InstallSecretStore: true, ResumeSecretsFile: path}
	if err := prepareInstallInputs(t.Context(), &cfg, InstallDeps{Output: &output}); err != nil {
		t.Fatal(err)
	}
	defer clear(cfg.resumeRootToken)
	if string(cfg.resumeRootToken) != token {
		t.Fatal("root token not loaded")
	}
	if bytes.Contains(output.Bytes(), []byte(token)) {
		t.Fatal("recovery input escaped to output")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readResumeRootToken(t.Context(), path, ""); err == nil {
		t.Fatal("public recovery file accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readResumeRootToken(t.Context(), link, ""); err == nil {
		t.Fatal("linked recovery input accepted")
	}
	if err := os.WriteFile(path, []byte("OPENBAO_ROOT_TOKEN="+token+"\nOPENBAO_ROOT_TOKEN="+token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readResumeRootToken(t.Context(), path, ""); err == nil || strings.Contains(err.Error(), token) {
		t.Fatal("duplicate root token was accepted or disclosed")
	}
}

func TestExistingSSHBlockUsesSelectedPort(t *testing.T) {
	input := []byte("Host other\n Port 22\nHost oberth\n HostName localhost\n Port 30022\n IdentityFile ~/.ssh/key\n Port 1234\nMatch host other\n Port 2222\n")
	got := updateSSHBlockPort(input, "oberth", "31022")
	want := "Host other\n Port 22\nHost oberth\n    Port 31022\n HostName localhost\n IdentityFile ~/.ssh/key\nMatch host other\n Port 2222\n"
	if string(got) != want {
		t.Fatalf("wrong SSH port update: %s", got)
	}
	if !bytes.Equal(got, updateSSHBlockPort(got, "oberth", "31022")) {
		t.Fatal("SSH update is not idempotent")
	}
}

func TestResumeEncryptedInputKeepsDecryptionPrivate(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "age")
	// Stand-in decryptor checks the actual descriptor/argv contract. Its output
	// is consumed privately, as age's plaintext would be.
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n[ \"$#\" = 3 ] && [ \"$1\" = --decrypt ] && [ \"$2\" = --identity ] && [ \"$3\" = /dev/fd/3 ] || exit 4\ncat <&3\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+"/usr/bin:/bin")
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	secret := hex.EncodeToString(random)
	identity := filepath.Join(root, "identity")
	ciphertext := filepath.Join(root, "encrypted")
	if err := os.WriteFile(identity, []byte("OPENBAO_ROOT_TOKEN="+secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ciphertext, []byte("encrypted fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readResumeRootToken(t.Context(), ciphertext, identity)
	defer clear(got)
	if err != nil || string(got) != secret {
		t.Fatal("private decryption contract failed")
	}
}

func TestSSHSetupUpdatesValidHostHeaderForms(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH is a test prerequisite")
	}
	for _, header := range []string{"Host\toberth", "Host=oberth", `Host "oberth" # managed alias`} {
		t.Run(header, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".ssh"), 0700); err != nil {
				t.Fatal(err)
			}
			key := filepath.Join(root, ".ssh", "client")
			if err := os.WriteFile(key, nil, 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, ".ssh", "config")
			body := header + "\n HostName localhost\n Port 30022\n IdentityFile old-key\nMatch\thost other\n Port 2222\nHost other\n IdentityFile other-key\n"
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			deps := Deps{Output: &output, UserHomeDir: func() (string, error) { return root, nil }, ClientSSHPort: "31022"}
			if err := offerSSHConfig(t.Context(), deps, key+".pub", newTableWriter(&output, false)); err != nil {
				t.Fatal(err)
			}
			updated, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(updated, []byte("Match\thost other\n Port 2222\nHost other\n IdentityFile other-key")) {
				t.Fatal("unrelated SSH sections changed")
			}
			command := exec.CommandContext(t.Context(), ssh, "-G", "-F", path, "oberth")
			command.Stderr = io.Discard
			resolved, err := command.Output()
			if err != nil {
				t.Fatal("OpenSSH rejected the updated configuration")
			}
			if !bytes.Contains(resolved, []byte("port 31022\n")) || !bytes.Contains(resolved, []byte("identityfile "+key+"\n")) {
				t.Fatal("OpenSSH did not select the updated port and identity")
			}
		})
	}
}

func TestResumeEncryptedInputWithRealAge(t *testing.T) {
	for _, tool := range []string{"age", "age-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("age is a test prerequisite")
		}
	}
	root := t.TempDir()
	identity := filepath.Join(root, "identity")
	command := exec.CommandContext(t.Context(), "age-keygen", "-o", identity)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		t.Fatal("age identity creation failed")
	}
	recipient, err := exec.CommandContext(t.Context(), "age-keygen", "-y", identity).Output()
	if err != nil {
		t.Fatal("age recipient read failed")
	}
	file := filepath.Join(root, "recovery.age")
	sink, err := createSecretsFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.enableEncryption(t.Context(), strings.TrimSpace(string(recipient))); err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(random)
	sink.add("Root token", token)
	var output bytes.Buffer
	sink.finish(&output)
	if sink.err != nil {
		t.Fatal("recovery encryption failed")
	}
	got, err := readResumeRootToken(t.Context(), file, identity)
	defer clear(got)
	if err != nil || string(got) != token {
		t.Fatal("encrypted recovery did not round trip")
	}
	if bytes.Contains(output.Bytes(), got) {
		t.Fatal("root token appeared in output")
	}
}
