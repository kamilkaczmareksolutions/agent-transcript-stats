package stats_test

import (
	"os/exec"
	"strings"
	"testing"
)

func stdLibPackages() map[string]struct{} {
	out, err := exec.Command("go", "list", "std").Output()
	if err != nil {
		panic(err)
	}
	m := map[string]struct{}{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			m[line] = struct{}{}
		}
	}
	return m
}

func TestNoNetwork(t *testing.T) {
	std := stdLibPackages()
	for _, pkg := range []string{".", "./cmd/stats"} {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, dep := range strings.Split(string(out), "\n") {
			dep = strings.TrimSpace(dep)
			if dep == "" || strings.HasPrefix(dep, "github.com/kamilkaczmareksolutions/agent-transcript-stats") {
				continue
			}
			if dep == "net" || strings.HasPrefix(dep, "net/") {
				t.Fatalf("package %s depends on %s", pkg, dep)
			}
			if _, ok := std[dep]; !ok {
				t.Fatalf("package %s depends on non-stdlib %s", pkg, dep)
			}
		}
	}
}
