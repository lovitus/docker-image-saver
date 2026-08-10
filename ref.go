package main

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
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

var (
	repositoryComponentPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$`)
	tagPattern                 = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

func parseImageRef(raw string) (imageRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return imageRef{}, fmt.Errorf("image reference is empty")
	}
	if strings.Count(raw, "@") > 1 || strings.ContainsAny(raw, `\\?#%`) || strings.IndexFunc(raw, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return imageRef{}, fmt.Errorf("invalid image reference: %s", raw)
	}

	ref := imageRef{}
	namePart := raw

	if at := strings.LastIndex(raw, "@"); at >= 0 {
		ref.Digest = strings.TrimSpace(raw[at+1:])
		namePart = strings.TrimSpace(raw[:at])
		if err := validateReferenceDigest(ref.Digest); err != nil {
			return imageRef{}, err
		}
	}
	if namePart == "" {
		return imageRef{}, fmt.Errorf("invalid image reference: %s", raw)
	}

	tag := ""
	lastSlash := strings.LastIndex(namePart, "/")
	lastColon := strings.LastIndex(namePart, ":")
	if lastColon > lastSlash {
		tag = strings.TrimSpace(namePart[lastColon+1:])
		namePart = strings.TrimSpace(namePart[:lastColon])
		if !tagPattern.MatchString(tag) {
			return imageRef{}, fmt.Errorf("invalid tag in reference: %s", raw)
		}
	}

	ref.Tag = tag
	if ref.Digest == "" {
		if tag == "" {
			ref.Tag = "latest"
		}
	}

	parts := strings.Split(namePart, "/")
	if len(parts) == 0 || namePart == "" {
		return imageRef{}, fmt.Errorf("invalid image reference: %s", raw)
	}

	repositoryParts := parts
	if isRegistryComponent(parts[0]) {
		if len(parts) < 2 || !validRegistryComponent(parts[0]) {
			return imageRef{}, fmt.Errorf("invalid registry in reference: %s", raw)
		}
		ref.Registry = parts[0]
		repositoryParts = parts[1:]
	} else {
		ref.Registry = dockerHubRegistryAlias
	}
	ref.Repository = strings.Join(repositoryParts, "/")
	if err := validateRepositoryPath(ref.Repository); err != nil {
		return imageRef{}, fmt.Errorf("invalid repository in reference %s: %w", raw, err)
	}

	if ref.Registry == dockerHubRegistryAlias && !strings.Contains(ref.Repository, "/") {
		ref.Repository = "library/" + ref.Repository
	}

	return ref, nil
}

func validRegistryComponent(value string) bool {
	return value != "" && !strings.Contains(value, "://") && !strings.ContainsAny(value, `\\/?#%@`)
}

func validateRepositoryPath(repository string) error {
	if repository == "" {
		return fmt.Errorf("repository is empty")
	}
	for _, component := range strings.Split(repository, "/") {
		if component == "" || component == "." || component == ".." || !repositoryComponentPattern.MatchString(component) {
			return fmt.Errorf("invalid repository component %q", component)
		}
	}
	return nil
}

func validateReferenceDigest(digest string) error {
	_, _, err := newDigestHasher(digest)
	return err
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
	if r.Digest != "" {
		return r.DisplayRepository() + "@" + r.Digest
	}
	tag := r.Tag
	if tag == "" {
		tag = "latest"
	}
	return r.DisplayRepository() + ":" + tag
}

func isRegistryComponent(part string) bool {
	return strings.Contains(part, ".") || strings.Contains(part, ":") || part == "localhost"
}
