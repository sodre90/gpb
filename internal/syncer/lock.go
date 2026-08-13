package syncer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrRunInProgress means another process is already syncing. Two runs sharing one pool would
// race on the same .part files and double the request rate against Google, so the second one
// declines rather than joining in.
var ErrRunInProgress = errors.New("another sync run is already in progress")

// AcquireRunLock takes an exclusive advisory lock for the duration of a run. The daemon has
// an in-process guard of its own; this covers the case that guard cannot see, which is a
// second gpb process — `podman exec gpb gpb sync` against a running daemon.
//
// The returned release closes the file, which the kernel also does if the process dies, so a
// crashed run cannot leave the lock held forever.
func AcquireRunLock(path string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the run lock: %w", err)
	}

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, ErrRunInProgress
	}

	return func() {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}, nil
}
