package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockDataDir takes an exclusive advisory lock on the data directory and
// returns a function that releases it.
//
// HopReact keeps its state in SQLite and drives its polling from an
// in-process ticker, so two instances against the same directory would each
// evaluate every watch and each queue its own notifications — every user
// would get doubled alerts. That is exactly the kind of fault nobody spots
// until people complain, so it is refused up front instead.
func lockDataDir(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".hopreact.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	// LOCK_NB so a second instance fails immediately with a clear message
	// rather than hanging with no explanation.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
