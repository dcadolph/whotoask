package index

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/whotoask/internal/connector"
)

// scopedPerson returns a person record of a source and scope with one topic.
func scopedPerson(source, scope, email, topic string) connector.Record {
	return connector.Record{
		Kind: connector.KindPerson, Source: source, Scope: scope,
		Name: email, Email: email, Topics: []string{topic}, Weight: 1,
	}
}

// heldEmails returns the emails a source holds, in held order.
func heldEmails(ix *Index, source string) []string {
	var out []string
	for _, rec := range ix.SourceRecords(source) {
		out = append(out, rec.Email)
	}
	return out
}

// TestAddKeepsOtherScopes verifies a read replaces what its own scope of a
// source held and keeps the source's other scopes. Keyed by source alone, a
// second file or repository read into the same source replaced the first.
func TestAddKeepsOtherScopes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Steps      [][]connector.Record
		WantResult []string
	}{{ // Test 0: A second file is kept beside the first.
		Steps: [][]connector.Record{
			{scopedPerson("org-csv", "file:a.csv", "alice@x.com", "billing")},
			{scopedPerson("org-csv", "file:b.csv", "bob@x.com", "payments")},
		},
		WantResult: []string{"alice@x.com", "bob@x.com"},
	}, { // Test 1: Reading a file again replaces only that file.
		Steps: [][]connector.Record{
			{scopedPerson("org-csv", "file:a.csv", "alice@x.com", "billing")},
			{scopedPerson("org-csv", "file:b.csv", "bob@x.com", "payments")},
			{scopedPerson("org-csv", "file:a.csv", "amy@x.com", "billing")},
		},
		WantResult: []string{"amy@x.com", "bob@x.com"},
	}, { // Test 2: A read with no scope stands for the whole source and replaces all of it.
		Steps: [][]connector.Record{
			{scopedPerson("org-csv", "file:a.csv", "alice@x.com", "billing")},
			{scopedPerson("org-csv", "file:b.csv", "bob@x.com", "payments")},
			{scopedPerson("org-csv", "", "cy@x.com", "ledger")},
		},
		WantResult: []string{"cy@x.com"},
	}, { // Test 3: Records held with no scope give way to any read of the source.
		Steps: [][]connector.Record{
			{scopedPerson("org-csv", "", "old@x.com", "ledger")},
			{scopedPerson("org-csv", "file:a.csv", "alice@x.com", "billing")},
		},
		WantResult: []string{"alice@x.com"},
	}, { // Test 4: Another source is left alone.
		Steps: [][]connector.Record{
			{scopedPerson("org-csv", "file:a.csv", "alice@x.com", "billing")},
			{scopedPerson("matters", "file:a.csv", "bob@x.com", "payments")},
		},
		WantResult: []string{"alice@x.com"},
	}, { // Test 5: Held order follows the scope, whatever order the runs came in.
		Steps: [][]connector.Record{
			{scopedPerson("org-csv", "file:b.csv", "bob@x.com", "payments")},
			{scopedPerson("org-csv", "file:a.csv", "alice@x.com", "billing")},
		},
		WantResult: []string{"alice@x.com", "bob@x.com"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ix := New()
			for _, recs := range test.Steps {
				ix.Add(recs)
			}
			if diff := cmp.Diff(test.WantResult, heldEmails(ix, "org-csv"), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("held org-csv records (-want +got):\n%s", diff)
			}
			if got := ix.SourceSize("org-csv"); got != len(test.WantResult) {
				t.Errorf("SourceSize = %d, want %d", got, len(test.WantResult))
			}
		})
	}
}

// TestMergeIncrementalFoldsWithinItsScope verifies a delta folds into the held
// record of the same person in the same scope, and leaves that person's record
// in another scope of the source untouched.
func TestMergeIncrementalFoldsWithinItsScope(t *testing.T) {
	t.Parallel()
	ix := New()
	ix.Add([]connector.Record{scopedPerson("git", "repo:/a", "alice@x.com", "billing")})
	ix.Add([]connector.Record{scopedPerson("git", "repo:/b", "alice@x.com", "payments")})
	ix.MergeIncremental([]connector.Record{scopedPerson("git", "repo:/b", "alice@x.com", "ledger")})

	want := map[string][]string{"repo:/a": {"billing"}, "repo:/b": {"payments", "ledger"}}
	got := make(map[string][]string)
	for _, rec := range ix.SourceRecords("git") {
		got[rec.Scope] = append(got[rec.Scope], rec.Topics...)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("held git topics by scope (-want +got):\n%s", diff)
	}
}

// TestScopesReplacesAndDropScopes verifies the three calls a caller uses to
// decide what a full read takes the place of.
func TestScopesReplacesAndDropScopes(t *testing.T) {
	t.Parallel()
	ix := New()
	ix.Add([]connector.Record{
		scopedPerson("git", "repo:/a", "alice@x.com", "billing"),
		scopedPerson("git", "repo:/a", "amy@x.com", "billing"),
	})
	ix.Add([]connector.Record{scopedPerson("git", "repo:/b", "bob@x.com", "payments")})

	if diff := cmp.Diff([]string{"repo:/a", "repo:/b"}, ix.Scopes("git")); diff != "" {
		t.Errorf("Scopes (-want +got):\n%s", diff)
	}
	tests := []struct {
		Scopes     map[string]bool
		WantResult int
	}{{ // Test 0: A read of one scope replaces that scope's records.
		Scopes: map[string]bool{"repo:/a": true}, WantResult: 2,
	}, { // Test 1: A read of a scope the source does not hold replaces nothing.
		Scopes: map[string]bool{"repo:/c": true}, WantResult: 0,
	}, { // Test 2: A read with no scope replaces everything.
		Scopes: map[string]bool{"": true}, WantResult: 3,
	}}
	for testNum, test := range tests {
		if got := ix.Replaces("git", test.Scopes); got != test.WantResult {
			t.Errorf("test %d: Replaces = %d, want %d", testNum, got, test.WantResult)
		}
	}
	if got := ix.DropScopes("git", func(scope string) bool { return scope == "repo:/a" }); got != 2 {
		t.Errorf("DropScopes removed %d, want 2", got)
	}
	if diff := cmp.Diff([]string{"bob@x.com"}, heldEmails(ix, "git")); diff != "" {
		t.Errorf("held after DropScopes (-want +got):\n%s", diff)
	}
	if got := ix.SourceSize("git"); got != 1 {
		t.Errorf("SourceSize after DropScopes = %d, want 1", got)
	}
}
