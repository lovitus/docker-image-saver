package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var version = "dev"

type cliOptions struct {
	Image    string
	Output   string
	Proxy    string
	Arch     string
	Username string
	Password string
	Insecure bool
	Version  bool
}

func main() {
	if len(os.Args) == 1 {
		if err := runWizard(version); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if isWizardCommand(os.Args[1:]) {
		if err := runWizard(version); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	opts, err := parseCLI(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	if opts.Version {
		fmt.Println(version)
		return
	}

	if opts.Image == "" {
		fmt.Fprintln(os.Stderr, "error: image reference is required (use --image or positional argument)")
		os.Exit(2)
	}
	if opts.Output == "" {
		defaultOut, derr := defaultOutputTar(opts.Image)
		if derr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", derr)
			os.Exit(2)
		}
		opts.Output = defaultOut
	}

	runOpts := runOptions{
		Image:    opts.Image,
		Output:   opts.Output,
		Arch:     opts.Arch,
		Proxy:    opts.Proxy,
		Username: opts.Username,
		Password: opts.Password,
		Insecure: opts.Insecure,
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
	}
	if err := runNonInteractive(runOpts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func parseCLI(args []string) (cliOptions, error) {
	var opts cliOptions
	args = normalizeCLIArgs(args)
	args = reorderArgsForFlagSet(args)

	fs := flag.NewFlagSet("dia", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.Image, "image", "", "image reference (example: alpine:latest)")
	fs.StringVar(&opts.Output, "output", "", "output tar path")
	fs.StringVar(&opts.Arch, "arch", "", "architecture selection indices (example: 1,2,5-)")
	fs.StringVar(&opts.Proxy, "proxy", "", "explicit proxy URL (supports http(s):// and socks5://)")
	fs.StringVar(&opts.Username, "username", "", "registry username")
	fs.StringVar(&opts.Password, "password", "", "registry password")
	fs.BoolVar(&opts.Insecure, "insecure", false, "skip TLS verification")
	fs.BoolVar(&opts.Version, "version", false, "print version")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}

	if opts.Image == "" {
		remaining := fs.Args()
		if len(remaining) > 0 {
			opts.Image = strings.TrimSpace(remaining[0])
		}
		if opts.Output == "" && len(remaining) > 1 {
			opts.Output = strings.TrimSpace(remaining[1])
		}
	}

	if opts.Password == "" {
		opts.Password = os.Getenv("DIA_REGISTRY_PASSWORD")
	}
	if opts.Username == "" {
		opts.Username = os.Getenv("DIA_REGISTRY_USERNAME")
	}
	return opts, nil
}

func normalizeCLIArgs(args []string) []string {
	if len(args) == 0 {
		return args
	}
	cmd := strings.ToLower(strings.TrimSpace(args[0]))
	switch cmd {
	case "pull", "save":
		return args[1:]
	default:
		return args
	}
}

func isWizardCommand(args []string) bool {
	if len(args) != 1 {
		return false
	}
	cmd := strings.ToLower(strings.TrimSpace(args[0]))
	return cmd == "wizard" || cmd == "interactive"
}

func reorderArgsForFlagSet(args []string) []string {
	if len(args) == 0 {
		return args
	}
	expectsValue := map[string]bool{
		"--image":    true,
		"--output":   true,
		"--arch":     true,
		"--proxy":    true,
		"--username": true,
		"--password": true,
	}
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}

		flags = append(flags, arg)
		name := arg
		if eq := strings.Index(name, "="); eq >= 0 {
			name = name[:eq]
		}
		if expectsValue[name] && !strings.Contains(arg, "=") && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}

	return append(flags, positional...)
}

func defaultOutputTar(image string) (string, error) {
	ref, err := parseImageRef(image)
	if err != nil {
		return "", err
	}
	name := strings.ReplaceAll(ref.Repository, "/", "_")
	tag := ref.Tag
	if tag == "" {
		tag = "latest"
	}
	file := fmt.Sprintf("%s_%s.tar", name, tag)
	if ref.Registry != dockerHubRegistryAlias {
		prefix := strings.ReplaceAll(ref.Registry, ":", "_")
		file = fmt.Sprintf("%s_%s", prefix, file)
	}
	if strings.TrimSpace(file) == "" {
		return "", errors.New("unable to generate output path")
	}
	return filepath.Clean(file), nil
}
