package installer

import (
	"bytes"
	"path/filepath"

	"github.com/oberthci/oberth/internal/clientcert"
)

func (e *Engine) savedAdministrator(p *Plan) (admin, error) {
	old, err := e.Credentials.profile()
	p.previous = old
	if newAdministrator(old, err) {
		a, err := e.administrator(p.identities, nil)
		a.action = "Create local administrator key"
		return a, err
	}
	if err != nil {
		return admin{}, described("custody", err, e.Credentials.path)
	}
	ca, _ := clientcert.ParseCertificate(p.identities.CA.Data["tls.crt"])
	if old.CAPin != clientcert.Pin(ca) {
		return admin{}, described("profile-ca", nil, "Profile directory: "+e.Credentials.path)
	}
	raw, err := e.Credentials.read(filepath.Base(old.AdminKey))
	if err != nil {
		return admin{}, described("custody", err, old.AdminKey)
	}
	a, err := e.administrator(p.identities, raw)
	if err != nil {
		clear(raw)
		return admin{}, err
	}
	a.action = "Reuse administrator key; refresh local certificate"
	return a, nil
}

func adminMatches(a admin, data map[string][]byte) bool {
	return bytes.Equal(a.public, data["admin.pub"]) && bytes.Equal(a.public, data["push.pub"])
}

func newAdministrator(old Profile, err error) bool {
	return isAbsent(err) || err == nil && old.answersOnly()
}
