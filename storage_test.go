package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMountForPathUsesLongestContainingMount(t *testing.T) {
	root := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	data := filepath.Join(root, "srv", "data")
	target := filepath.Join(data, "team", "archives")
	got := mountForPath(target, []filesystemMount{{Path: root}, {Path: filepath.Join(root, "srv")}, {Path: data}})
	if got != filepath.Clean(data) {
		t.Fatalf("unexpected mount: got %q want %q", got, filepath.Clean(data))
	}
}

func TestSelectSyncOutputRootCreatesNewDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-yet-created", "archives")
	selected, err := selectSyncOutputRoot(syncJobSpec{OutputRoot: root})
	if err != nil {
		t.Fatalf("select explicit output root: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat created output root: %v", err)
	}
	if !info.IsDir() || selected.Path != root || !selected.Recommended {
		t.Fatalf("unexpected selected storage: %+v", selected)
	}
}
