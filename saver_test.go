package main

import "testing"

func TestPerPlatformOutputsSingleSelection(t *testing.T) {
	platforms := []platformOption{{Platform: platform{OS: "linux", Architecture: "amd64"}}}
	selected := []int{0}
	outputs, err := perPlatformOutputs("/tmp/image.tar", platforms, selected)
	if err != nil {
		t.Fatalf("perPlatformOutputs returned error: %v", err)
	}
	if len(outputs) != 1 {
		t.Fatalf("expected 1 output, got %d", len(outputs))
	}
	if outputs[0] != "/tmp/image.tar" {
		t.Fatalf("unexpected output path: %s", outputs[0])
	}
}

func TestPerPlatformOutputsMultiSelection(t *testing.T) {
	platforms := []platformOption{
		{Platform: platform{OS: "linux", Architecture: "amd64"}},
		{Platform: platform{OS: "linux", Architecture: "arm64", Variant: "v8"}},
	}
	selected := []int{0, 1}
	outputs, err := perPlatformOutputs("/tmp/image.tar", platforms, selected)
	if err != nil {
		t.Fatalf("perPlatformOutputs returned error: %v", err)
	}
	if len(outputs) != 2 {
		t.Fatalf("expected 2 outputs, got %d", len(outputs))
	}
	if outputs[0] != "/tmp/image_linux_amd64.tar" {
		t.Fatalf("unexpected first output path: %s", outputs[0])
	}
	if outputs[1] != "/tmp/image_linux_arm64_v8.tar" {
		t.Fatalf("unexpected second output path: %s", outputs[1])
	}
}
