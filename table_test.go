package stats

import (
	"testing"
)

func TestSourceTableMatchesConstants(t *testing.T) {
	want := map[Source]bool{
		Claude: false,
		Codex:  false,
		Cursor: true,
	}
	for _, spec := range sourceTable {
		exp, ok := want[spec.source]
		if !ok {
			t.Fatalf("unexpected source in table: %q", spec.source)
		}
		if spec.experimental != exp {
			t.Fatalf("%s experimental: got %v want %v", spec.source, spec.experimental, exp)
		}
		delete(want, spec.source)
	}
	if len(want) != 0 {
		t.Fatalf("missing sources in table: %v", want)
	}
}
