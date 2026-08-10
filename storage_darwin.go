//go:build darwin

package main

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

func discoverFilesystemMounts() ([]filesystemMount, error) {
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, fmt.Errorf("count mounted filesystems: %w", err)
	}
	stats := make([]unix.Statfs_t, count)
	count, err = unix.Getfsstat(stats, unix.MNT_NOWAIT)
	if err != nil {
		return nil, fmt.Errorf("list mounted filesystems: %w", err)
	}
	mounts := make([]filesystemMount, 0, count)
	for _, stat := range stats[:count] {
		mountPath := byteCString(stat.Mntonname[:])
		filesystemType := byteCString(stat.Fstypename[:])
		if mountPath == "" || isDarwinVirtualFilesystem(filesystemType) {
			continue
		}
		mounts = append(mounts, filesystemMount{
			Path:     mountPath,
			ReadOnly: stat.Flags&unix.MNT_RDONLY != 0,
		})
	}
	return mounts, nil
}

func filesystemSpace(path string) (uint64, uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	blockSize := uint64(stat.Bsize)
	return uint64(stat.Blocks) * blockSize, uint64(stat.Bavail) * blockSize, nil
}

func byteCString(value []byte) string {
	for index, item := range value {
		if item == 0 {
			return string(value[:index])
		}
	}
	return string(value)
}

func isDarwinVirtualFilesystem(filesystemType string) bool {
	switch strings.ToLower(filesystemType) {
	case "devfs", "autofs", "volfs":
		return true
	default:
		return false
	}
}
