package store

import "os"

// statFile returns the size of one file, used for the archive and its WAL.
func statFile(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
