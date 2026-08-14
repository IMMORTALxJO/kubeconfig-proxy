package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"time"
)

type runtimeFileSnapshot struct {
	checksum [sha256.Size]byte
}

func readRuntimeFileSnapshot(path string) (runtimeFileSnapshot, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- runtime file paths come from the explicit state path and its validated source kubeconfig path.
	if err != nil {
		return runtimeFileSnapshot{}, err
	}
	return runtimeFileSnapshot{checksum: sha256.Sum256(data)}, nil
}

func (s runtimeFileSnapshot) isEqual(other runtimeFileSnapshot) bool {
	return s.checksum == other.checksum
}

func watchRuntimeFiles(
	ctx context.Context,
	statePath string,
	stateSnapshot runtimeFileSnapshot,
	sourceKubeconfigPath string,
	sourceKubeconfigSnapshot runtimeFileSnapshot,
) <-chan error {
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
				nextState, err := readRuntimeFileSnapshot(statePath)
				if err != nil {
					if os.IsNotExist(err) {
						sendRuntimeFileEvent(ctx, changed, stateFileRemovedError(statePath))
						return
					}
					sendRuntimeFileEvent(ctx, changed, fmt.Errorf("read state file %s: %w", statePath, err))
					return
				}
				nextSourceKubeconfig, err := readRuntimeFileSnapshot(sourceKubeconfigPath)
				if err != nil {
					sendRuntimeFileEvent(ctx, changed, fmt.Errorf("read source kubeconfig %s: %w", sourceKubeconfigPath, err))
					return
				}
				if !sourceKubeconfigSnapshot.isEqual(nextSourceKubeconfig) {
					nextSourceKubeconfig, err = waitForStableRuntimeFileSnapshot(ctx, sourceKubeconfigPath, nextSourceKubeconfig)
					if ctx.Err() != nil {
						return
					}
					if err != nil {
						sendRuntimeFileEvent(ctx, changed, fmt.Errorf("read source kubeconfig %s: %w", sourceKubeconfigPath, err))
						return
					}
				}
				if !sourceKubeconfigSnapshot.isEqual(nextSourceKubeconfig) {
					sourceKubeconfigSnapshot = nextSourceKubeconfig
					if !sendRuntimeFileEvent(ctx, changed, errSourceKubeconfigChanged) {
						return
					}
					continue
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
