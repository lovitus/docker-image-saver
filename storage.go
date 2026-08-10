package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type filesystemMount struct {
	Path     string
	ReadOnly bool
}

type storageCandidate struct {
	Path           string `json:"path"`
	MountPoint     string `json:"mount_point"`
	AvailableBytes uint64 `json:"available_bytes"`
	TotalBytes     uint64 `json:"total_bytes"`
	Recommended    bool   `json:"recommended,omitempty"`
}

func probeArchiveStorage(workspace string, configuredRoots []string) ([]storageCandidate, error) {
	workspace, err := expandUserPath(workspace)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("workspace is required")
	}
	mounts, err := discoverFilesystemMounts()
	if err != nil {
		return nil, err
	}
	if len(mounts) == 0 {
		mounts = []filesystemMount{{Path: filepath.VolumeName(workspace) + string(filepath.Separator)}}
	}

	roots := make([]string, 0, len(configuredRoots)+len(mounts)+2)
	for _, configured := range configuredRoots {
		expanded, expandErr := expandUserPath(configured)
		if expandErr != nil {
			return nil, expandErr
		}
		if expanded != "" {
			roots = append(roots, expanded)
		}
	}
	roots = append(roots, filepath.Join(workspace, "archives"))
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		roots = append(roots, filepath.Join(home, ".local", "share", "dia", "archives"))
	}
	for _, mount := range mounts {
		if mount.ReadOnly || !directoryWritable(mount.Path) {
			continue
		}
		roots = append(roots, filepath.Join(mount.Path, "dia-archives"))
	}

	roots = cleanUniquePaths(roots)
	candidates := make([]storageCandidate, 0, len(roots))
	for _, root := range roots {
		ancestor, ancestorErr := nearestExistingDirectory(root)
		if ancestorErr != nil || !directoryWritable(ancestor) {
			continue
		}
		mount := mountForPath(ancestor, mounts)
		total, available, statErr := filesystemSpace(ancestor)
		if statErr != nil {
			continue
		}
		candidates = append(candidates, storageCandidate{
			Path:           filepath.Clean(root),
			MountPoint:     mount,
			AvailableBytes: available,
			TotalBytes:     total,
		})
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no writable archive storage directory found")
	}
	slices.SortFunc(candidates, func(a, b storageCandidate) int {
		switch {
		case a.AvailableBytes > b.AvailableBytes:
			return -1
		case a.AvailableBytes < b.AvailableBytes:
			return 1
		case len(a.Path) < len(b.Path):
			return -1
		case len(a.Path) > len(b.Path):
			return 1
		default:
			return strings.Compare(a.Path, b.Path)
		}
	})
	candidates[0].Recommended = true
	return candidates, nil
}

func selectArchiveStorage(workspace string, configuredRoots []string) (storageCandidate, error) {
	candidates, err := probeArchiveStorage(workspace, configuredRoots)
	if err != nil {
		return storageCandidate{}, err
	}
	selected := candidates[0]
	if err := os.MkdirAll(selected.Path, 0o755); err != nil {
		return storageCandidate{}, fmt.Errorf("create archive storage directory %s: %w", selected.Path, err)
	}
	return selected, nil
}

func directoryWritable(dir string) bool {
	file, err := os.CreateTemp(dir, ".dia-write-probe-*")
	if err != nil {
		return false
	}
	name := file.Name()
	_ = file.Close()
	_ = os.Remove(name)
	return true
}

func mountForPath(target string, mounts []filesystemMount) string {
	target = filepath.Clean(target)
	best := ""
	for _, mount := range mounts {
		candidate := filepath.Clean(mount.Path)
		rel, err := filepath.Rel(candidate, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if len(candidate) > len(best) {
			best = candidate
		}
	}
	if best == "" {
		best = filepath.VolumeName(target) + string(filepath.Separator)
	}
	return best
}

func cleanUniquePaths(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = filepath.Clean(strings.TrimSpace(value))
		if value == "." || value == "" {
			continue
		}
		key := value
		if fold, err := shouldFoldReservationPathCase(value); err == nil && fold {
			key = strings.ToLower(value)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}

func expandUserPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if value == "~" || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if value == "~" {
			value = home
		} else {
			value = filepath.Join(home, value[2:])
		}
	}
	abs, err := filepath.Abs(filepath.Clean(value))
	if err != nil {
		return "", fmt.Errorf("resolve storage path: %w", err)
	}
	return abs, nil
}
