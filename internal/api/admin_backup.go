package api

import (
	"errors"
	"io"
	"net/http"
	"time"
)

const adminBackupWriteIdle = 30 * time.Second

// Renew the connection deadline per chunk instead of applying the server's
// three-minute request write timeout to an arbitrarily large verified archive.
func streamAdminBackup(w http.ResponseWriter, source io.Reader) error {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			if err := controller.SetWriteDeadline(time.Now().Add(adminBackupWriteIdle)); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return err
			}
			sent, err := w.Write(buffer[:n])
			if err != nil {
				return err
			}
			if sent != n {
				return io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
