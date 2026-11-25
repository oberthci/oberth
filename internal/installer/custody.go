package installer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// Profile contains public endpoints, identity and credential paths, never keys.
type Profile struct {
	Namespace         string `json:"namespace"`
	PipelineNamespace string `json:"pipeline_namespace,omitempty"`
	HTTPSNodePort     int    `json:"https_node_port,omitempty"`
	HTTPSServicePort  int    `json:"https_service_port,omitempty"`
	SSHNodePort       int    `json:"ssh_node_port,omitempty"`
	RecoveryKey       string `json:"recovery_key,omitempty"`
	Version           int    `json:"version"`
	ServerVersion     string `json:"server_version"`
	Context           string `json:"context"`
	ClusterUID        string `json:"cluster_uid"`
	ServerURL         string `json:"server_url"`
	MCPURL            string `json:"mcp_url"`
	SSHURL            string `json:"ssh_url"`
	CAPin             string `json:"ca_sha256"`
	AdminKey          string `json:"admin_key"`
	AdminCert         string `json:"admin_certificate"`
	CAFile            string `json:"ca_file"`
	KnownHosts        string `json:"known_hosts"`
	BackupRecipient   string `json:"backup_recipient"`
	BackupPublicKey   string `json:"backup_public_key"`
	BackupKey         string `json:"backup_key"`
	BootstrapPending  bool   `json:"bootstrap_pending"`
	// Ready records that an apply of this profile's installation reached a
	// ready server; a later apply is an update of a completed installation.
	Ready bool `json:"ready,omitempty"`
}

// ConfigDir confines credentials to an owner-only directory through os.Root.
type ConfigDir struct {
	operation     sync.Mutex
	operationHeld bool
	root          *os.Root
	directory     *os.File
	path          string
}

func OpenConfigDir(path string) (*ConfigDir, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, problem("custody", err)
	}
	if err = credentialPath(path); err != nil {
		return nil, err
	}
	if _, err = os.Stat(path); isAbsent(err) {
		return &ConfigDir{path: path}, nil
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, problem("custody", err)
	}
	if err = credentialPath(path); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, problem("custody", err)
	}
	directory, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, problem("custody", err)
	}
	c := &ConfigDir{root: root, directory: directory, path: path}
	st, err := root.Stat(".")
	if err != nil || !private(st, true) {
		_ = c.Close()
		return nil, problem("custody", err)
	}
	return c, nil
}
func private(st os.FileInfo, dir bool) bool {
	if st == nil || st.IsDir() != dir || st.Mode().Perm()&0077 != 0 {
		return false
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uint32(os.Geteuid()) && (dir || st.Mode().IsRegular())
}
func (c *ConfigDir) Close() error {
	if c.root == nil {
		return nil
	}
	return errors.Join(c.root.Close(), c.directory.Close())
}
func (c *ConfigDir) pathFor(name string) string { return filepath.Join(c.path, name) }
func (c *ConfigDir) read(name string) ([]byte, error) {
	if c.root == nil {
		return nil, &os.PathError{Op: "open", Path: c.pathFor(name), Err: os.ErrNotExist}
	}
	f, err := c.root.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !private(st, false) {
		return nil, problem("custody", err)
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if len(b) > 1<<20 {
		return nil, problem("custody", nil)
	}
	return b, err
}
func (c *ConfigDir) save(name string, b []byte) error {
	if err := c.create(); err != nil {
		return err
	}
	f, err := c.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		old, e := c.read(name)
		defer clear(old)
		if e != nil || !bytes.Equal(old, b) {
			return problem("custody", e)
		}
		return nil
	}
	if err != nil {
		return problem("custody", err)
	}
	_, writeErr := f.Write(b)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return problem("custody", err)
	}
	return c.directory.Sync()
}

// replace publishes b under name through a synced temporary file and a rename,
// so a crash never leaves a partial file under name.
func (c *ConfigDir) replace(name, temp string, b []byte) error {
	if err := c.save(temp, b); err != nil {
		return err
	}
	if err := c.root.Rename(temp, name); err != nil {
		return problem("custody", err)
	}
	return c.directory.Sync()
}

func isAbsent(err error) bool { return errors.Is(err, os.ErrNotExist) }
func (c *ConfigDir) saveProfile(p Profile, id string) error {
	if b, err := c.read("installation.json"); err == nil {
		var old Profile
		if json.Unmarshal(b, &old) != nil || old.ClusterUID != p.ClusterUID || old.Context != p.Context {
			return problem("custody", nil)
		}
	} else if !isAbsent(err) {
		return problem("custody", err)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	name := "profile-" + id + ".json"
	if err = c.save(name, b); err != nil {
		return err
	}
	// Publish the complete profile only after every referenced credential is synced.
	if err = c.root.Rename(name, "installation.json"); err != nil {
		return err
	}
	return c.directory.Sync()
}

func credentialPath(path string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return problem("custody", err)
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return problem("custody", err)
	}
	home, _ := os.UserHomeDir()
	home, _ = filepath.EvalSymlinks(home)
	for dir := cwd; dir != filepath.Dir(dir) && dir != home; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			rel, err := filepath.Rel(dir, path)
			if err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return described("custody", err, "Profile directory is inside the current checkout: "+path)
			}
			break
		}
	}
	return nil
}

// Creation is deferred until an approved operation needs to persist credentials.
func (c *ConfigDir) create() error {
	if c.root != nil {
		return nil
	}
	if err := os.MkdirAll(c.path, 0700); err != nil {
		return problem("custody", err)
	}
	opened, err := OpenConfigDir(c.path)
	if err != nil {
		return err
	}
	if c.operationHeld {
		if err := flock(opened.directory, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = opened.Close()
			return err
		}
	}
	c.root, c.directory, c.path = opened.root, opened.directory, opened.path
	return nil
}
