package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dcadolph/whotoask/internal/connector"
)

// heldNames returns the sorted names a source holds, read from the sources file
// beside the index.
func heldNames(t *testing.T, dir, source string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "index.sources.json"))
	if err != nil {
		t.Fatalf("read sources: %v", err)
	}
	var side struct {
		Sources map[string][]connector.Record `json:"sources"`
	}
	if err := json.Unmarshal(raw, &side); err != nil {
		t.Fatalf("decode sources: %v", err)
	}
	var names []string
	for _, rec := range side.Sources[source] {
		names = append(names, rec.Name)
	}
	slices.Sort(names)
	return names
}

// TestSecondFileKeepsTheFirst verifies a second file merged into the same
// source is held beside the first, and that a refresh, which replays both
// runs, ends with both. Before, the index kept one record set per source: the
// second file replaced the first one's people, and a refresh replayed the
// first file and then lost it to the second again.
func TestSecondFileKeepsTheFirst(t *testing.T) {
	t.Parallel()
	dir, files := t.TempDir(), t.TempDir()
	a, b := filepath.Join(files, "a.csv"), filepath.Join(files, "b.csv")
	header := "name,email,title,team,org,manager,topics\n"
	write := func(path, rows string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(header+rows), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(a, "Alice Able,alice@x.com,Engineer,Payments,Eng,,billing\n"+
		"Amy Ash,amy@x.com,Engineer,Payments,Eng,,payments\n")
	write(b, "Bob Birch,bob@x.com,Engineer,Ledger,Eng,,ledger\n"+
		"Ben Bay,ben@x.com,Engineer,Ledger,Eng,,ledger\n")
	want := []string{"Alice Able", "Amy Ash", "Ben Bay", "Bob Birch"}

	// Test 0: The second file is merged in and both are held.
	steps := [][]string{
		{"index", "--source", "org-csv", "--file", a},
		{"index", "--source", "org-csv", "--file", b, "--merge"},
	}
	for stepNum, step := range steps {
		if _, errOut, err := runCmd(t, append(step, "--data-dir", dir)...); err != nil {
			t.Fatalf("step %d: %v\n%s", stepNum, err, errOut)
		}
	}
	if diff := cmp.Diff(want, heldNames(t, dir, "org-csv")); diff != "" {
		t.Errorf("held after the second file (-want +got):\n%s", diff)
	}

	// Test 1: A refresh replays both files and still ends with both.
	if _, errOut, err := runCmd(t, "refresh", "--data-dir", dir); err != nil {
		t.Fatalf("refresh: %v\n%s", err, errOut)
	}
	if diff := cmp.Diff(want, heldNames(t, dir, "org-csv")); diff != "" {
		t.Errorf("held after a refresh (-want +got):\n%s", diff)
	}

	// Test 2: Reading the first file again after it changed replaces only it.
	write(a, "Alice Able,alice@x.com,Engineer,Payments,Eng,,billing\n")
	if _, errOut, err := runCmd(t, "index", "--source", "org-csv", "--file", a, "--merge",
		"--allow-shrink", "--data-dir", dir); err != nil {
		t.Fatalf("re-read: %v\n%s", err, errOut)
	}
	if diff := cmp.Diff([]string{"Alice Able", "Ben Bay", "Bob Birch"}, heldNames(t, dir, "org-csv")); diff != "" {
		t.Errorf("held after the first file changed (-want +got):\n%s", diff)
	}
}
