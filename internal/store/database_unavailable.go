package store

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// IsDatabaseUnavailable reports SQLite's "unable to open database file":
// the database path or its directory does not exist here. Administrative
// commands that read the server database hit it when run outside the pod.
func IsDatabaseUnavailable(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqlite3.SQLITE_CANTOPEN
}
