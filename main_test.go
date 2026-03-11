package main

import "testing"

func TestParseImageRef(t *testing.T) {
	tests := []struct {
		in           string
		registry     string
		repository   string
		tag          string
		displayRepo  string
		displayTag   string
		expectDigest string
	}{
		{
			in:          "alpine",
			registry:    dockerHubRegistryAlias,
			repository:  "library/alpine",
			tag:         "latest",
			displayRepo: "alpine",
			displayTag:  "alpine:latest",
		},
		{
			in:          "ghcr.io/example/app:v1",
			registry:    "ghcr.io",
			repository:  "example/app",
			tag:         "v1",
			displayRepo: "ghcr.io/example/app",
			displayTag:  "ghcr.io/example/app:v1",
		},
		{
			in:           "docker.io/library/busybox@sha256:abc",
			registry:     dockerHubRegistryAlias,
			repository:   "library/busybox",
			tag:          "",
			displayRepo:  "busybox",
			displayTag:   "busybox:latest",
			expectDigest: "sha256:abc",
		},
	}

	for _, tt := range tests {
		ref, err := parseImageRef(tt.in)
		if err != nil {
			t.Fatalf("parseImageRef(%q): %v", tt.in, err)
		}
		if ref.Registry != tt.registry || ref.Repository != tt.repository || ref.Tag != tt.tag {
			t.Fatalf("parseImageRef(%q) mismatch: got %+v", tt.in, ref)
		}
		if ref.DisplayRepository() != tt.displayRepo {
			t.Fatalf("DisplayRepository(%q): got %q want %q", tt.in, ref.DisplayRepository(), tt.displayRepo)
		}
		if ref.DisplayTag() != tt.displayTag {
			t.Fatalf("DisplayTag(%q): got %q want %q", tt.in, ref.DisplayTag(), tt.displayTag)
		}
		if tt.expectDigest != "" && ref.Digest != tt.expectDigest {
			t.Fatalf("digest mismatch for %q: got %q want %q", tt.in, ref.Digest, tt.expectDigest)
		}
	}
}

func TestSelectPlatforms(t *testing.T) {
	selected, err := selectPlatforms("1,3-4,6-", 7)
	if err != nil {
		t.Fatalf("selectPlatforms returned error: %v", err)
	}
	expected := []int{0, 2, 3, 5, 6}
	if len(selected) != len(expected) {
		t.Fatalf("unexpected length: got %v want %v", selected, expected)
	}
	for i := range expected {
		if selected[i] != expected[i] {
			t.Fatalf("unexpected selection: got %v want %v", selected, expected)
		}
	}
}

func TestParseCLIWithSubcommandAndTrailingFlags(t *testing.T) {
	opts, err := parseCLI([]string{
		"pull",
		"alpine:latest",
		"/tmp/alpine.tar",
		"--arch", "1,2",
		"--proxy", "socks5://127.0.0.1:1080",
	})
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if opts.Image != "alpine:latest" {
		t.Fatalf("unexpected image: %q", opts.Image)
	}
	if opts.Output != "/tmp/alpine.tar" {
		t.Fatalf("unexpected output: %q", opts.Output)
	}
	if opts.Arch != "1,2" {
		t.Fatalf("unexpected arch: %q", opts.Arch)
	}
	if opts.Proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("unexpected proxy: %q", opts.Proxy)
	}
}
