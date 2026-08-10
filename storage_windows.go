//go:build windows

package main

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func discoverFilesystemMounts() ([]filesystemMount, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, fmt.Errorf("list Windows drives: %w", err)
	}
	mounts := make([]filesystemMount, 0)
	for index := 0; index < 26; index++ {
		if mask&(1<<index) == 0 {
			continue
		}
		mounts = append(mounts, filesystemMount{Path: string(rune('A'+index)) + `:\`})
	}
	return mounts, nil
}

func filesystemSpace(path string) (uint64, uint64, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var available uint64
	var total uint64
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(pathPointer, &available, &total, &free); err != nil {
		return 0, 0, err
	}
	return total, available, nil
}
