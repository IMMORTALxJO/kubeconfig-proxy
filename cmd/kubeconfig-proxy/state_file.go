package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

type stateFileSnapshot struct {
	modTime time.Time
	size    int64
}

func readStateFileSnapshot(path string) (stateFileSnapshot, error) {
	info, err := os.Stat(path)
	if err != nil {
		return stateFileSnapshot{}, err
	}
	return stateFileSnapshot{modTime: info.ModTime(), size: info.Size()}, nil
}

func (s stateFileSnapshot) equal(other stateFileSnapshot) bool {
	return s.size == other.size && s.modTime.Equal(other.modTime)
}

func watchStateFile(ctx context.Context, path string, snapshot stateFileSnapshot) <-chan error {
	changed := make(chan error, 1)
	ticker := time.NewTicker(statePollInterval)
	go func() {
		defer ticker.Stop()
		defer close(changed)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				next, err := readStateFileSnapshot(path)
				if err != nil {
					if os.IsNotExist(err) {
						changed <- stateFileRemovedError(path)
						return
					}
					changed <- fmt.Errorf("stat state file %s: %w", path, err)
					return
				}
				if !snapshot.equal(next) {
					changed <- nil
					return
				}
			}
		}
	}()
	return changed
}

func stateFileRemovedError(path string) error {
	return fmt.Errorf("state file disappeared: %s: %w", path, errStateFileRemoved)
}
