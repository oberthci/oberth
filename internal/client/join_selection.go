package client

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/model"
	"os"
)

// Installation metadata distinguishes an install administrator from a joined
// uplink. This is local profile selection, never server-side authorization.
func joinSelection(base string, invitation model.InvitationFile) (Profile, bool, error) {
	result := Profile{ServerURL: invitation.ServerURL, Uplink: true}
	current, dir, err := loadProfile(base, "")
	if err != nil {
		return result, false, err
	}
	if dir == "" || !apicontract.SameServerAddress(current.ServerURL, invitation.ServerURL) {
		return result, false, nil
	}
	raw, err := ReadPublic(profilePath(current.CAFile, dir, "ca.crt"))
	if err != nil {
		return result, false, err
	}
	ca, err := clientcert.ParseCertificate(raw)
	if err != nil {
		return result, false, failure("profile", err)
	}
	if clientcert.Pin(ca) != invitation.CAPin {
		return result, false, nil
	}
	return result, current.AdminKey != "" && current.AdminCert != "", nil
}

func commitJoinSelection(root *os.Root, temp string, keep bool) error {
	if keep {
		return nil
	}
	if err := root.Rename(temp, "current"); err != nil {
		return err
	}
	return syncDirectory(root)
}
