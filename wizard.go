package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func runWizard(version string) error {
	in := bufio.NewReader(os.Stdin)
	out := os.Stdout

	fmt.Fprintf(out, "dia (docker-image-saver) %s\n", version)
	fmt.Fprintln(out, "Wizard Mode")
	fmt.Fprintln(out, "This wizard will save an image directly from registry API v2 into a docker-load tar.")
	fmt.Fprintln(out)

	image, err := promptRequiredWithDefault(in, out, "1) Image (name:tag)", "alpine:latest")
	if err != nil {
		return err
	}
	if _, err := parseImageRef(image); err != nil {
		return err
	}

	defaultOut, err := defaultOutputTar(image)
	if err != nil {
		return err
	}
	outputPath, err := promptWithDefault(in, out, "2) Output .tar path", defaultOut)
	if err != nil {
		return err
	}
	if outputPath == "" {
		outputPath = defaultOut
	}
	outputPath = filepath.Clean(outputPath)

	proxy, err := promptWithDefault(in, out, "3) Proxy URL (optional)", "")
	if err != nil {
		return err
	}
	username, err := promptWithDefault(in, out, "4) Registry username (optional)", strings.TrimSpace(os.Getenv("DIA_REGISTRY_USERNAME")))
	if err != nil {
		return err
	}
	password, err := promptWithDefault(in, out, "5) Registry password (optional)", strings.TrimSpace(os.Getenv("DIA_REGISTRY_PASSWORD")))
	if err != nil {
		return err
	}
	insecure, err := promptYesNo(in, out, "6) Insecure TLS?", false)
	if err != nil {
		return err
	}

	ref, err := parseImageRef(image)
	if err != nil {
		return err
	}
	client, err := newRegistryClient(proxy, username, password, insecure)
	if err != nil {
		return err
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "7) Fetching manifest list and available architectures...")
	platforms, singleManifest, err := resolvePlatforms(client, ref)
	if err != nil {
		return err
	}
	for i, p := range platforms {
		fmt.Fprintf(out, "  %d. %s\n", i+1, p.Platform.String())
	}

	archSelection, err := promptWithDefault(in, out, "8) Select architectures (e.g. 1,2,5- or all)", "all")
	if err != nil {
		return err
	}
	selected, err := selectPlatforms(archSelection, len(platforms))
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		return fmt.Errorf("no architectures selected")
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	for _, idx := range selected {
		fmt.Fprintf(out, "Selected [%d] %s\n", idx+1, platforms[idx].Platform.String())
	}
	if err := writeDockerTar(client, ref, singleManifest, platforms, selected, outputPath, out); err != nil {
		return err
	}

	result, err := summarizeSavedFile(outputPath)
	if err != nil {
		return err
	}
	printSaveResult(out, result)
	return nil
}

func promptWithDefault(in *bufio.Reader, out io.Writer, label, defaultValue string) (string, error) {
	if strings.TrimSpace(defaultValue) == "" {
		fmt.Fprintf(out, "%s: ", label)
	} else {
		fmt.Fprintf(out, "%s [%s]: ", label, defaultValue)
	}
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return strings.TrimSpace(defaultValue), nil
	}
	return line, nil
}

func promptRequiredWithDefault(in *bufio.Reader, out io.Writer, label, defaultValue string) (string, error) {
	for {
		value, err := promptWithDefault(in, out, label, defaultValue)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(value) != "" {
			return value, nil
		}
		fmt.Fprintln(out, "Input is required.")
	}
}

func promptYesNo(in *bufio.Reader, out io.Writer, label string, defaultValue bool) (bool, error) {
	defaultText := "y/N"
	if defaultValue {
		defaultText = "Y/n"
	}
	for {
		fmt.Fprintf(out, "%s [%s]: ", label, defaultText)
		line, err := in.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "" {
			return defaultValue, nil
		}
		switch line {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(out, "Please input y/yes or n/no.")
		}
	}
}
