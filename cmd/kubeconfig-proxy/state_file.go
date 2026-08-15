package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"time"
)

type runtimeFileSnapshot struct {
	exists   bool
	checksum [sha256.Size]byte
}

func readRuntimeFileSnapshot(path string) (runtimeFileSnapshot, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- runtime paths come from the explicit state path or client-go kubeconfig loading rules.
	if err != nil {
		return runtimeFileSnapshot{}, err
	}
	return runtimeFileSnapshot{exists: true, checksum: sha256.Sum256(data)}, nil
}

func (s runtimeFileSnapshot) isEqual(other runtimeFileSnapshot) bool {
	return s.exists == other.exists && s.checksum == other.checksum
}

type watchedRuntimeFile struct {
	path     string
	snapshot runtimeFileSnapshot
}

func snapshotKubeconfigFiles(paths []string) ([]watchedRuntimeFile, error) {
	files := make([]watchedRuntimeFile, 0, len(paths))
	seenPaths := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seenPaths[path]; ok {
			continue
		}
		seenPaths[path] = struct{}{}
		snapshot, err := readOptionalRuntimeFileSnapshot(path)
		if err != nil {
			return nil, fmt.Errorf("read source kubeconfig %s: %w", path, err)
		}
		files = append(files, watchedRuntimeFile{path: path, snapshot: snapshot})
	}
	return files, nil
}

func readOptionalRuntimeFileSnapshot(path string) (runtimeFileSnapshot, error) {
	snapshot, err := readRuntimeFileSnapshot(path)
	if os.IsNotExist(err) {
		return runtimeFileSnapshot{}, nil
	}
	return snapshot, err
}

func waitForStableOptionalRuntimeFileSnapshot(ctx context.Context, path string, snapshot runtimeFileSnapshot) (runtimeFileSnapshot, error) {
	timer := time.NewTimer(runtimeFileSettleInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return runtimeFileSnapshot{}, ctx.Err()
		case <-timer.C:
			next, err := readOptionalRuntimeFileSnapshot(path)
			if err != nil {
				return runtimeFileSnapshot{}, err
			}
			if snapshot.isEqual(next) {
				return next, nil
			}
			snapshot = next
			timer.Reset(runtimeFileSettleInterval)
		}
	}
}

func watchRuntimeFiles(
	ctx context.Context,
	statePath string,
	stateSnapshot runtimeFileSnapshot,
	kubeconfigFiles []watchedRuntimeFile,
) <-chan error {
	changed := make(chan error, 1)
	ticker := time.NewTicker(statePollInterval)
	go func() {
		defer ticker.Stop()
		defer close(changed)
	watchLoop:
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				nextState, err := readRuntimeFileSnapshot(statePath)
				if err != nil {
					if os.IsNotExist(err) {
						sendRuntimeFileEvent(ctx, changed, stateFileRemovedError(statePath))
						return
					}
					sendRuntimeFileEvent(ctx, changed, fmt.Errorf("read state file %s: %w", statePath, err))
					return
				}
				for i := range kubeconfigFiles {
					nextKubeconfig, err := readOptionalRuntimeFileSnapshot(kubeconfigFiles[i].path)
					if err != nil {
						sendRuntimeFileEvent(ctx, changed, fmt.Errorf("read source kubeconfig %s: %w", kubeconfigFiles[i].path, err))
						return
					}
					if kubeconfigFiles[i].snapshot.isEqual(nextKubeconfig) {
						continue
					}
					nextKubeconfig, err = waitForStableOptionalRuntimeFileSnapshot(ctx, kubeconfigFiles[i].path, nextKubeconfig)
					if ctx.Err() != nil {
						return
					}
					if err != nil {
						sendRuntimeFileEvent(ctx, changed, fmt.Errorf("read source kubeconfig %s: %w", kubeconfigFiles[i].path, err))
						return
					}
					if !kubeconfigFiles[i].snapshot.isEqual(nextKubeconfig) {
						kubeconfigFiles[i].snapshot = nextKubeconfig
						if !sendRuntimeFileEvent(ctx, changed, errSourceKubeconfigChanged) {
							return
						}
						continue watchLoop
					}
				}
				if !stateSnapshot.isEqual(nextState) {
					nextState, err = waitForStableRuntimeFileSnapshot(ctx, statePath, nextState)
					if ctx.Err() != nil {
						return
					}
					if err != nil {
						sendRuntimeFileEvent(ctx, changed, fmt.Errorf("read state file %s: %w", statePath, err))
						return
					}
				}
				if !stateSnapshot.isEqual(nextState) {
					stateSnapshot = nextState
					if !sendRuntimeFileEvent(ctx, changed, errStateFileChanged) {
						return
					}
				}
			}
		}
	}()
	return changed
}

func sendRuntimeFileEvent(ctx context.Context, changed chan<- error, err error) bool {
	select {
	case changed <- err:
		return true
	case <-ctx.Done():
		return false
	}
}

func waitForStableRuntimeFileSnapshot(ctx context.Context, path string, snapshot runtimeFileSnapshot) (runtimeFileSnapshot, error) {
	timer := time.NewTimer(runtimeFileSettleInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return runtimeFileSnapshot{}, ctx.Err()
		case <-timer.C:
			next, err := readRuntimeFileSnapshot(path)
			if err != nil {
				return runtimeFileSnapshot{}, err
			}
			if snapshot.isEqual(next) {
				return next, nil
			}
			snapshot = next
			timer.Reset(runtimeFileSettleInterval)
		}
	}
}

func stateFileRemovedError(path string) error {
	return fmt.Errorf("state file disappeared: %s: %w", path, errStateFileRemoved)
}
