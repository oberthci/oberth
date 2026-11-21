package secretstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/runtimeconfig"
)

// ServerFiles is the store portion of the server configuration's files object.
// Projected files may be symlinks; the kubelet replaces their targets on rotation.
type ServerFiles struct {
	SecretStoreCA    string `json:"secret_store_ca"`
	SecretStoreToken string `json:"secret_store_token"`
}

// ServerWriter uses the mounted CA and reopens the projected JWT for every write.
// Connection.URL/ServerName and Config come from the same server configuration.
func ServerWriter(ctx context.Context, connection Connection, cfg Config, files ServerFiles) (Writer, runtimeconfig.SecretDelivery, error) {
	if err := cfg.Validate(); err != nil {
		return Writer{}, runtimeconfig.SecretDelivery{}, refuse("configuration", nil)
	}
	ca, err := ReadMountedFile(files.SecretStoreCA, runtimeconfig.SecretCABytes, storeCAField)
	if err != nil {
		return Writer{}, runtimeconfig.SecretDelivery{}, err
	}
	connection.CA = ca
	client, err := New(connection)
	if err != nil {
		var public *publicerr.Error
		if runtimeconfig.ValidStoreAddress(connection.URL) && errors.As(err, &public) {
			public.Detail = storeCAField + ": invalid trust configuration"
		}
		return Writer{}, runtimeconfig.SecretDelivery{}, err
	}
	token := func(ctx context.Context) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return ReadMountedFile(files.SecretStoreToken, 64<<10, storeTokenField)
	}
	jwt, err := token(ctx)
	clear(jwt)
	if err != nil {
		client.Close()
		return Writer{}, runtimeconfig.SecretDelivery{}, err
	}
	return Writer{Client: client, Config: cfg, Token: token}, runtimeconfig.SecretDelivery{Address: connection.URL, ServerName: connection.ServerName, CAPEM: string(ca), Access: runtimeconfig.SecretStoreAccess(cfg)}, nil
}

// ReadMountedFile reads a configured projection through a bounded regular target.
func ReadMountedFile(path string, limit int64, field string) ([]byte, error) {
	return ReadMountedFileChecked(path, limit, field, nil)
}

// ReadMountedFileChecked validates the opened regular target before reading its bytes.
func ReadMountedFileChecked(path string, limit int64, field string, check func(os.FileInfo) error) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, mountedRefusal(field, "absolute")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, mountedRefusal(field, "missing")
		}
		return nil, mountedRefusal(field, "unreadable")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, mountedRefusal(field, "unreadable")
	}
	if !info.Mode().IsRegular() {
		return nil, mountedRefusal(field, "regular")
	}
	if check != nil {
		if err := check(info); err != nil {
			return nil, err
		}
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		clear(value)
		return nil, mountedRefusal(field, "unreadable")
	}
	if len(value) == 0 {
		return nil, mountedRefusal(field, "empty")
	}
	if int64(len(value)) > limit {
		clear(value)
		return nil, mountedRefusal(field, "large")
	}
	return value, nil
}

const storeCAField = "files.secret_store_ca"
const storeTokenField = "files.secret_store_token"

func mountedRefusal(field, state string) error {
	reason, remedy := fileRefusal(field, state)
	return publicerr.New(publicerr.InvalidInput, reason, remedy, nil)
}
