//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func discoverFilesystemMounts() ([]filesystemMount, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("read mount information: %w", err)
	}
	defer file.Close()
	mounts := make([]filesystemMount, 0)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 6 || separator+1 >= len(fields) {
			continue
		}
		filesystemType := fields[separator+1]
		if isVirtualFilesystem(filesystemType) {
			continue
		}
		mountPath := decodeMountInfoPath(fields[4])
		options := strings.Split(fields[5], ",")
		mounts = append(mounts, filesystemMount{Path: mountPath, ReadOnly: containsString(options, "ro")})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan mount information: %w", err)
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

func decodeMountInfoPath(value string) string {
	var result strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+3 < len(value) {
			if decoded, err := strconv.ParseUint(value[i+1:i+4], 8, 8); err == nil {
				result.WriteByte(byte(decoded))
				i += 3
				continue
			}
		}
		result.WriteByte(value[i])
	}
	return result.String()
}

func isVirtualFilesystem(filesystemType string) bool {
	switch filesystemType {
	case "proc", "sysfs", "devtmpfs", "devpts", "tmpfs", "cgroup", "cgroup2", "securityfs", "pstore", "debugfs", "tracefs", "configfs", "mqueue", "hugetlbfs", "rpc_pipefs", "fusectl", "autofs", "binfmt_misc":
		return true
	default:
		return false
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
