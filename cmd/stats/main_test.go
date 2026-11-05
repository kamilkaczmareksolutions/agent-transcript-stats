package main_test

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIExamples(t *testing.T) {
	bin := buildCLI(t)
	examples := filepath.Join("..", "..", "examples")
	cmd := exec.Command(bin, examples)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v stderr=%q", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr not empty: %q", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "sessions") || !strings.Contains(out, "3") {
		t.Fatalf("expected table output, got %q", out)
	}
	if strings.Contains(out, "sk-abcdefghijklmnopqrstuvwxyz012345") {
		t.Fatal("secret leaked to stdout")
	}
}

func TestCLIJSON(t *testing.T) {
	bin := buildCLI(t)
	examples := filepath.Join("..", "..", "examples")
	cmd := exec.Command(bin, "--json", examples)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Sessions int `json:"sessions"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatal(err)
	}
	if report.Sessions != 3 {
		t.Fatalf("sessions: got %d want 3", report.Sessions)
	}
	if strings.Contains(string(out), "sk-abcdefghijklmnopqrstuvwxyz012345") {
		t.Fatal("secret in json stdout")
	}
}

func TestCLIBadFlags(t *testing.T) {
	bin := buildCLI(t)
	cmd := exec.Command(bin, "--json", "--md", "examples")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected error")
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("expected exit 2, got %v", err)
	}
}

func buildCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "stats")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/stats")
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	return bin
}
