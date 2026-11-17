package abi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The TinyOS syscall table is not generated here and must not be read from a
// TinyOS checkout: goc32 has to build on its own, so the table it was written
// against is a committed source file with its provenance in the header
// (ARCHITECTURE-NEXT 3.1).
//
// This test is the half that works with only goc32 present: the file has to
// describe itself.  The other half - "does it still match the real table" -
// lives in the TinyOS tree as internal/build/syscallsnapshot_test.go, because
// that is the only place both files exist.
func TestSnapshotIsSelfConsistent(t *testing.T) {
	raw, err := os.ReadFile("syscalls.snapshot.csv")
	if err != nil {
		t.Fatalf("missing snapshot: %v", err)
	}

	header, body := splitHeader(string(raw))

	commit := field(header, "tinyos_commit")
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		t.Errorf("tinyos_commit must be a full sha1, got %q", commit)
	}

	want := field(header, "tinyos_csv_sha256")
	sum := sha256.Sum256([]byte(body))
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("body does not match tinyos_csv_sha256\n got: %s\nwant: %s", got, want)
	}

	rows := parse(t, body)
	if len(rows) < 20 {
		t.Fatalf("only %d syscalls in the snapshot; the parse is probably wrong", len(rows))
	}

	seen := map[string]int{}
	last := -1
	for i, r := range rows {
		if prev, dup := seen[r.name]; dup {
			t.Errorf("line %d: %s already defined as %d", i+2, r.name, prev)
		}
		seen[r.name] = r.value
		if r.value <= last {
			t.Errorf("line %d: %s = %d does not increase (previous %d); the table is append-only",
				i+2, r.name, r.value, last)
		}
		last = r.value
	}

	// SYS_EXIT is the one number every program depends on, and it is the one
	// an edit is most likely to renumber by accident.
	if v, ok := seen["SYS_EXIT"]; !ok || v != 0 {
		t.Errorf("SYS_EXIT must be 0, got %d (present: %v)", v, ok)
	}
}

type row struct {
	name  string
	value int
}

func splitHeader(text string) (string, string) {
	var head, body strings.Builder
	inBody := false
	for _, line := range strings.SplitAfter(text, "\n") {
		if !inBody && strings.HasPrefix(line, "#") {
			head.WriteString(line)
			continue
		}
		inBody = true
		body.WriteString(line)
	}
	return head.String(), body.String()
}

func field(header, key string) string {
	re := regexp.MustCompile(`(?m)^#\s+` + regexp.QuoteMeta(key) + `:\s*(\S+)\s*$`)
	m := re.FindStringSubmatch(header)
	if m == nil {
		return ""
	}
	return m[1]
}

func parse(t *testing.T, body string) []row {
	t.Helper()
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) == 0 || lines[0] != "name,value" {
		t.Fatalf("first body line must be name,value, got %q", lines[0])
	}
	var out []row
	for i, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			t.Fatalf("line %d: want name,value, got %q", i+2, line)
		}
		v, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			t.Fatalf("line %d: %v", i+2, fmt.Errorf("%s: %w", line, err))
		}
		out = append(out, row{name: strings.TrimSpace(parts[0]), value: v})
	}
	return out
}
