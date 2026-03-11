package main

import (
	"fmt"
	"path"
	"strings"
)

const (
	dockerHubRegistryAlias = "docker.io"
	dockerHubRegistryHost  = "registry-1.docker.io"
)

type imageRef struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
}

func parseImageRef(raw string) (imageRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return imageRef{}, fmt.Errorf("image reference is empty")
	}

	ref := imageRef{}
	namePart := raw

	if at := strings.LastIndex(raw, "@"); at >= 0 {
		ref.Digest = strings.TrimSpace(raw[at+1:])
		namePart = strings.TrimSpace(raw[:at])
	}

	tag := ""
	lastSlash := strings.LastIndex(namePart, "/")
	lastColon := strings.LastIndex(namePart, ":")
	if lastColon > lastSlash {
		tag = strings.TrimSpace(namePart[lastColon+1:])
		namePart = strings.TrimSpace(namePart[:lastColon])
	}

	if ref.Digest == "" {
		if tag == "" {
			tag = "latest"
		}
		ref.Tag = tag
	}

	parts := strings.Split(namePart, "/")
	if len(parts) == 0 {
		return imageRef{}, fmt.Errorf("invalid image reference: %s", raw)
	}

	if isRegistryComponent(parts[0]) {
		ref.Registry = parts[0]
		ref.Repository = strings.Join(parts[1:], "/")
	} else {
		ref.Registry = dockerHubRegistryAlias
		ref.Repository = namePart
	}

	ref.Repository = path.Clean(strings.TrimPrefix(ref.Repository, "/"))
	if ref.Repository == "." || ref.Repository == "" {
		return imageRef{}, fmt.Errorf("invalid repository in reference: %s", raw)
	}

	if ref.Registry == dockerHubRegistryAlias && !strings.Contains(ref.Repository, "/") {
		ref.Repository = "library/" + ref.Repository
	}

	return ref, nil
}

func (r imageRef) RegistryHost() string {
	if r.Registry == dockerHubRegistryAlias {
		return dockerHubRegistryHost
	}
	return r.Registry
}

func (r imageRef) ManifestReference() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

func (r imageRef) DisplayRepository() string {
	if r.Registry == dockerHubRegistryAlias {
		return strings.TrimPrefix(r.Repository, "library/")
	}
	return r.Registry + "/" + r.Repository
}

func (r imageRef) DisplayTag() string {
	tag := r.Tag
	if tag == "" {
		tag = "latest"
	}
	return r.DisplayRepository() + ":" + tag
}

func isRegistryComponent(part string) bool {
	return strings.Contains(part, ".") || strings.Contains(part, ":") || part == "localhost"
}
