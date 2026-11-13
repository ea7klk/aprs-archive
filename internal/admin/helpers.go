package admin

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aprs-archive/internal/store"
)

func itoa(n int) string     { return strconv.Itoa(n) }
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func baseName(p string) string { return filepath.Base(p) }

// validBackupName is the one validation a download path needs.
func validBackupName(name string) bool {
	return name != "" &&
		!strings.ContainsAny(name, `/\`) &&
		!strings.HasPrefix(name, ".") &&
		strings.HasSuffix(name, ".db")
}

func openBackup(dbPath, name string) (*os.File, error) {
	if !validBackupName(name) {
		return nil, fmt.Errorf("invalid backup name")
	}
	return os.Open(filepath.Join(store.BackupDir(dbPath), name))
}

// parseDay parses the date a form field produces.
func parseDay(v string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(v))
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
