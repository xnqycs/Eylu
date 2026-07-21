//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package mcpclient

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func acquireCredentialFileLock(ctx context.Context, path string) (func() error, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() error {
				unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
				return errors.Join(unlockErr, file.Close())
			}, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN && err != unix.EINTR {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func secureCredentialDirectory(path string) error { return os.Chmod(path, 0o700) }
func secureCredentialFile(path string) error      { return os.Chmod(path, 0o600) }

func atomicReplaceCredentialFile(source, target string) error {
	return os.Rename(source, target)
}

func syncCredentialDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
