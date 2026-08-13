package syncer

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

// DiskFloor is the free space a run refuses to eat into. It is deliberately generous: the pool
// shares its filesystem with whatever else the machine keeps there, and a backup is the one
// process on it that will consume every byte it is given.
const DiskFloor = 8 << 30

// ErrDiskFull stops a run that would otherwise keep going until the filesystem refused it.
// Without this the end of a full disk is thousands of per-item write failures, each one
// counted against that item's retry budget, so a transient-looking error permanently poisons
// items that were never the problem.
var ErrDiskFull = fmt.Errorf("not enough free space to keep downloading")

// FreeBytes is what the pool's filesystem will still accept from this user. It measures the
// nearest existing ancestor of the directory asked about, because on a first run the pool does
// not exist yet — but the filesystem it is about to be created on does, and that is the one
// whose free space matters.
func FreeBytes(dir string) (uint64, error) {
	existing, err := nearestExisting(dir)
	if err != nil {
		return 0, err
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs(existing, &stat); err != nil {
		return 0, fmt.Errorf("checking free space on %s: %w", existing, err)
	}
	return uint64(stat.Bsize) * stat.Bavail, nil
}

func nearestExisting(dir string) (string, error) {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no existing directory above %s", dir)
		}
		dir = parent
	}
}

// roomToContinue reports whether the pool still has the floor free. A failure to measure is
// not a failure to continue: an unreadable statfs must not stop a backup that is working.
func (s *Syncer) roomToContinue() error {
	if s.options.MinFreeBytes <= 0 {
		return nil
	}

	free, err := FreeBytes(s.poolDir)
	if err != nil {
		log.Printf("syncer: %v", err)
		return nil
	}
	if free >= uint64(s.options.MinFreeBytes) {
		return nil
	}
	return fmt.Errorf("%w: %s free, keeping %s in reserve",
		ErrDiskFull, gigabytes(int64(free)), gigabytes(s.options.MinFreeBytes))
}

func gigabytes(bytes int64) string {
	return fmt.Sprintf("%.1f GB", float64(bytes)/(1<<30))
}
