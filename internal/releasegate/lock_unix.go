//go:build !windows

package releasegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func lockSourceCache(ctx context.Context, root string) (func(), error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, ".source.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, errors.Join(ctx.Err(), err)
		case <-ticker.C:
		}
	}
}
