package syncer

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestRunLockExcludesASecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.lock")

	release, err := AcquireRunLock(path)
	if err != nil {
		t.Fatalf("taking the run lock: %v", err)
	}

	if _, err := AcquireRunLock(path); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("a second holder got %v, want ErrRunInProgress", err)
	}

	release()

	second, err := AcquireRunLock(path)
	if err != nil {
		t.Fatalf("the lock stayed held after release: %v", err)
	}
	second()
}
