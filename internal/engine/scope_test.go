package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/whotoask/internal/connector"
)

// scopedPeople returns count person records of a source and scope, named after
// the scope so two scopes never share a person.
func scopedPeople(source, scope string, count int) []connector.Record {
	recs := make([]connector.Record, 0, count)
	for i := range count {
		email := fmt.Sprintf("p%d-%s@x.com", i, scope)
		recs = append(recs, connector.Record{
			Kind: connector.KindPerson, Source: source, Scope: scope,
			Name: email, Email: email, Topics: []string{"billing"}, Weight: 1,
		})
	}
	return recs
}

// heldScopes returns how many records the saved index holds under each scope
// of a source.
func heldScopes(t *testing.T, x *Indexer, source string) map[string]int {
	t.Helper()
	ix, err := x.loadIndex()
	if err != nil {
		t.Fatalf("loadIndex: %v", err)
	}
	if err := x.loadSources(ix); err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	out := make(map[string]int)
	for _, rec := range ix.SourceRecords(source) {
		out[rec.Scope]++
	}
	return out
}

// TestGitSupersede verifies what a full git run does to a set of repositories
// an earlier run read together: it replaces the set when it reads all of it
// again, keeps it when it reads none of it, and is refused when it reads only
// part, since either answer would lose or double something.
func TestGitSupersede(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Paths      []string
		Held       string
		WantResult bool
		Want       error
	}{{ // Test 0: The run reads the whole held set and more.
		Paths: []string{"/a", "/b", "/c"}, Held: "repo:/a,/b", WantResult: true,
	}, { // Test 1: The run reads none of the held set.
		Paths: []string{"/c"}, Held: "repo:/a,/b", WantResult: false,
	}, { // Test 2: The run reads only part of the held set.
		Paths: []string{"/a"}, Held: "repo:/a,/b", Want: ErrScopeOverlap,
	}, { // Test 3: The run reads part of the held set and something new.
		Paths: []string{"/a", "/c"}, Held: "repo:/a,/b", Want: ErrScopeOverlap,
	}, { // Test 4: A held set of one repository the run reads again.
		Paths: []string{"/a", "/b"}, Held: "repo:/a", WantResult: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := gitSupersede(test.Paths)(test.Held)
			if !errors.Is(err, test.Want) {
				t.Fatalf("error = %v, want %v", err, test.Want)
			}
			if got != test.WantResult {
				t.Errorf("drop = %v, want %v", got, test.WantResult)
			}
		})
	}
}

// TestFoldKeepsAndReplacesScopes folds full reads of a source one after another
// and verifies which held scopes each leaves. It is the engine half of the
// second-file fix: the index keeps each scope, and the rule for the source
// decides what a new read takes the place of.
func TestFoldKeepsAndReplacesScopes(t *testing.T) {
	t.Parallel()
	type step struct {
		Source    string
		Scope     string
		Count     int
		Supersede func(string) (bool, error)
		Want      error
	}
	keep := func(string) (bool, error) { return false, nil }
	replace := func(string) (bool, error) { return true, nil }
	tests := []struct {
		Steps      []step
		WantResult map[string]int
	}{{ // Test 0: A file source keeps every file.
		Steps: []step{
			{Source: "org-csv", Scope: "file:a.csv", Count: 2, Supersede: keep},
			{Source: "org-csv", Scope: "file:b.csv", Count: 2, Supersede: keep},
		},
		WantResult: map[string]int{"file:a.csv": 2, "file:b.csv": 2},
	}, { // Test 1: A small second file is not mistaken for the first file shrinking.
		Steps: []step{
			{Source: "org-csv", Scope: "file:a.csv", Count: 10, Supersede: keep},
			{Source: "org-csv", Scope: "file:b.csv", Count: 2, Supersede: keep},
		},
		WantResult: map[string]int{"file:a.csv": 10, "file:b.csv": 2},
	}, { // Test 2: The same file coming back far smaller is still refused.
		Steps: []step{
			{Source: "org-csv", Scope: "file:a.csv", Count: 10, Supersede: keep},
			{Source: "org-csv", Scope: "file:a.csv", Count: 2, Supersede: keep, Want: ErrShrunkSource},
		},
		WantResult: map[string]int{"file:a.csv": 10},
	}, { // Test 3: A source that holds one scope at a time replaces the old one.
		Steps: []step{
			{Source: "github", Scope: "repos:o/a", Count: 2, Supersede: replace},
			{Source: "github", Scope: "repos:o/a,o/b", Count: 3, Supersede: replace},
		},
		WantResult: map[string]int{"repos:o/a,o/b": 3},
	}, { // Test 4: Git keeps a second repository beside the first.
		Steps: []step{
			{Source: "git", Scope: "repo:/a", Count: 2, Supersede: gitSupersede([]string{"/a"})},
			{Source: "git", Scope: "repo:/b", Count: 2, Supersede: gitSupersede([]string{"/b"})},
		},
		WantResult: map[string]int{"repo:/a": 2, "repo:/b": 2},
	}, { // Test 5: Git replaces the sets a wider run reads again.
		Steps: []step{
			{Source: "git", Scope: "repo:/a", Count: 2, Supersede: gitSupersede([]string{"/a"})},
			{Source: "git", Scope: "repo:/b", Count: 2, Supersede: gitSupersede([]string{"/b"})},
			{Source: "git", Scope: "repo:/a,/b", Count: 4, Supersede: gitSupersede([]string{"/a", "/b"})},
		},
		WantResult: map[string]int{"repo:/a,/b": 4},
	}, { // Test 6: Git refuses a run that reads only part of a set, and keeps the set.
		Steps: []step{
			{Source: "git", Scope: "repo:/a,/b", Count: 4, Supersede: gitSupersede([]string{"/a", "/b"})},
			{
				Source: "git", Scope: "repo:/a", Count: 2, Supersede: gitSupersede([]string{"/a"}),
				Want: ErrScopeOverlap,
			},
		},
		WantResult: map[string]int{"repo:/a,/b": 4},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			x := &Indexer{Dir: Dir(t.TempDir()), Log: &bytes.Buffer{}}
			var source string
			for stepNum, s := range test.Steps {
				source = s.Source
				err := x.Fold(context.Background(), scopedPeople(s.Source, s.Scope, s.Count), FoldParams{
					Merge: true, Source: s.Source, Supersede: s.Supersede,
				})
				if !errors.Is(err, s.Want) {
					t.Fatalf("step %d: error = %v, want %v", stepNum, err, s.Want)
				}
			}
			if diff := cmp.Diff(test.WantResult, heldScopes(t, x, source), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("held scopes (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSupersedeRule verifies which rule a source gets: a file source keeps its
// other files, every other source holds one scope at a time, and a host that
// keeps one scope per source replaces the old scope whatever the source.
func TestSupersedeRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Source      string
		SingleScope bool
		WantResult  bool
	}{{ // Test 0: A file source keeps another file.
		Source: "org-csv", WantResult: false,
	}, { // Test 1: An API source replaces its other scope.
		Source: "github", WantResult: true,
	}, { // Test 2: Under a one-scope host a file source replaces the old file.
		Source: "org-csv", SingleScope: true, WantResult: true,
	}, { // Test 3: Under a one-scope host git replaces a set it does not read.
		Source: "git", SingleScope: true, WantResult: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			spec, ok := SourceByName(test.Source)
			if !ok {
				t.Fatalf("no source named %q", test.Source)
			}
			x := &Indexer{SingleScope: test.SingleScope}
			got, err := x.supersede(spec, []string{"/z"})("repo:/a,/b")
			if err != nil {
				t.Fatalf("supersede: %v", err)
			}
			if got != test.WantResult {
				t.Errorf("drop = %v, want %v", got, test.WantResult)
			}
		})
	}
}
