package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/kamilkaczmareksolutions/agent-transcript-stats"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := fs.Bool("json", false, "JSON output")
	mdOut := fs.Bool("md", false, "Markdown output")
	topN := fs.Int("top", 20, "top N words")
	var sources multiFlag
	fs.Var(&sources, "source", "source filter (claude, codex, cursor)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *jsonOut && *mdOut {
		fmt.Fprintln(os.Stderr, "stats: --json and --md cannot be combined")
		return 2
	}
	if *topN < 1 {
		fmt.Fprintln(os.Stderr, stats.ErrBadTopN)
		return 2
	}

	roots := fs.Args()
	if len(roots) == 0 {
		roots = defaultRoots()
	}

	var srcList []stats.Source
	for _, s := range sources {
		switch strings.ToLower(s) {
		case "claude":
			srcList = append(srcList, stats.Claude)
		case "codex":
			srcList = append(srcList, stats.Codex)
		case "cursor":
			srcList = append(srcList, stats.Cursor)
		default:
			fmt.Fprintln(os.Stderr, stats.ErrBadSource)
			return 2
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	report, err := stats.Collect(ctx, stats.Input{
		Roots:   roots,
		Sources: srcList,
		TopN:    *topN,
	})
	if err != nil {
		var missing *stats.MissingPathError
		if errors.As(err, &missing) {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	switch {
	case *jsonOut:
		b, err := report.JSON()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		os.Stdout.Write(b)
	case *mdOut:
		fmt.Print(report.Markdown())
	default:
		fmt.Print(report.Text())
	}
	return 0
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func defaultRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var roots []string
	claude := filepath.Join(home, ".claude")
	if dirExists(claude) {
		roots = append(roots, claude)
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	if dirExists(codexHome) {
		roots = append(roots, codexHome)
	}
	cursorRoot := os.Getenv("CURSOR_CONFIG_DIR")
	if cursorRoot == "" {
		cursorRoot = filepath.Join(home, ".cursor")
	}
	if dirExists(cursorRoot) {
		roots = append(roots, cursorRoot)
	}
	return roots
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
