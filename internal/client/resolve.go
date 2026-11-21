package client

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"os"
	"path/filepath"
	"strings"
)

// Options are explicit transport overrides; empty values inherit environment and profile.
type Options struct {
	URL, TokenFile, CAFile, ClientCert, ClientKey string
}

// Resolve selects flags, environment, then one installation-bound profile.
func Resolve(o Options, base string, env func(string) string) (Options, error) {
	fields := []struct {
		value *string
		env   string
	}{{&o.URL, "URL"}, {&o.TokenFile, "TOKEN_FILE"}, {&o.CAFile, "CA_FILE"}, {&o.ClientCert, "CLIENT_CERT"}, {&o.ClientKey, "CLIENT_KEY"}}
	for _, f := range fields {
		if *f.value == "" {
			*f.value = env("OBERTH_" + f.env)
		}
	}
	if base == "" || explicitCredentials(o) {
		return o, validateAddress(o.URL)
	}
	p, dir, err := loadProfile(base, env("OBERTH_PROFILE"))
	if err != nil {
		return Options{}, err
	}
	if err = bindProfileAuthority(&o, p, dir); err != nil {
		return Options{}, err
	}
	o = inheritProfile(o, p, dir)
	return o, validateAddress(o.URL)
}

func inheritProfile(o Options, p Profile, dir string) Options {
	if o.URL == "" {
		o.URL = p.ServerURL
	}
	if dir != "" && apicontract.SameServerAddress(o.URL, p.ServerURL) {
		inheritFile(&o.TokenFile, dir, "token")
		if o.CAFile == "" {
			o.CAFile = profilePath(p.CAFile, dir, "ca.crt")
		}
		inheritDeclaredFile(&o.ClientCert, p.AdminCert, dir, "admin.crt")
		inheritDeclaredFile(&o.ClientKey, p.AdminKey, dir, "admin.key")
	}
	return o
}

func inheritFile(value *string, dir, name string) {
	if *value == "" {
		path := filepath.Join(dir, name)
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			*value = path
		}
	}
}

func validateAddress(text string) error {
	if strings.TrimSpace(text) == "" {
		return failure("selected", nil)
	}
	u, err := apicontract.ParseServerAddress(text)
	if err != nil {
		return failure("url", err)
	}
	if u.Scheme == "http" {
		return failure("http", nil)
	}
	if u.Scheme != "https" {
		return failure("url", nil)
	}
	return nil
}

func explicitCredentials(o Options) bool {
	return o.URL != "" && o.CAFile != "" && (o.TokenFile != "" || o.ClientCert != "" && o.ClientKey != "")
}

func profilePath(declared, dir, name string) string {
	if declared != "" {
		return declared
	}
	return filepath.Join(dir, name)
}
func inheritDeclaredFile(value *string, declared, dir, name string) {
	if *value == "" && declared != "" {
		*value = declared
		return
	}
	inheritFile(value, dir, name)
}
