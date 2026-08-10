//go:build !linux && !darwin && !windows

package main

import "fmt"

func discoverFilesystemMounts() ([]filesystemMount, error) {
	return nil, nil
}

func filesystemSpace(path string) (uint64, uint64, error) {
	return 0, 0, fmt.Errorf("filesystem space probing is not supported on this platform")
}
