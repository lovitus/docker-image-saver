package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestPromptPasswordDoesNotPrintDefaultSecret(t *testing.T) {
	const secret = "environment-secret"
	var output bytes.Buffer
	value, err := promptPassword(bufio.NewReader(strings.NewReader("\n")), &output, "Password", secret, -1)
	if err != nil {
		t.Fatalf("prompt password: %v", err)
	}
	if value != secret {
		t.Fatalf("default password was not selected")
	}
	if strings.Contains(output.String(), secret) {
		t.Fatal("default password was printed in the prompt")
	}
}

func TestPromptPasswordAcceptsNonTTYInput(t *testing.T) {
	var output bytes.Buffer
	value, err := promptPassword(bufio.NewReader(strings.NewReader("typed-secret\n")), &output, "Password", "", -1)
	if err != nil {
		t.Fatalf("prompt password: %v", err)
	}
	if value != "typed-secret" {
		t.Fatalf("unexpected password value: %q", value)
	}
}

func TestPromptPasswordPreservesWhitespace(t *testing.T) {
	var output bytes.Buffer
	value, err := promptPassword(bufio.NewReader(strings.NewReader(" spaced secret \n")), &output, "Password", "", -1)
	if err != nil {
		t.Fatalf("prompt password: %v", err)
	}
	if value != " spaced secret " {
		t.Fatalf("password whitespace changed: %q", value)
	}
}
