package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestNoteTopicTime verifies each token's date only moves forward, empty
// tokens and undated activity are ignored, and other tokens keep their date.
func TestNoteTopicTime(t *testing.T) {
	t.Parallel()
	old := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		WantTimes map[string]map[string]time.Time
		Toks      []string
		At        time.Time
	}{{ // Test 0: A later date moves the token forward and leaves others alone.
		Toks: []string{"billing"}, At: late,
		WantTimes: map[string]map[string]time.Time{"ann": {"billing": late, "search": mid}},
	}, { // Test 1: An earlier date leaves the token where it was.
		Toks: []string{"billing", "search"}, At: old,
		WantTimes: map[string]map[string]time.Time{"ann": {"billing": mid, "search": mid}},
	}, { // Test 2: A new token is dated and an empty token is skipped.
		Toks: []string{"ledger", ""}, At: late,
		WantTimes: map[string]map[string]time.Time{"ann": {"billing": mid, "search": mid, "ledger": late}},
	}, { // Test 3: Undated activity dates nothing.
		Toks:      []string{"billing"},
		WantTimes: map[string]map[string]time.Time{"ann": {"billing": mid, "search": mid}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := map[string]map[string]time.Time{"ann": {"billing": mid, "search": mid}}
			noteTopicTime(got, "ann", test.Toks, test.At)
			if diff := cmp.Diff(test.WantTimes, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestMergeTopicTimes verifies merging keeps the later date of each subject
// and creates people and subjects the destination lacks.
func TestMergeTopicTimes(t *testing.T) {
	t.Parallel()
	old := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		Into      map[string]map[string]time.Time
		From      map[string]map[string]time.Time
		WantTimes map[string]map[string]time.Time
	}{{ // Test 0: The later date wins on both sides.
		Into:      map[string]map[string]time.Time{"ann": {"billing": old, "search": late}},
		From:      map[string]map[string]time.Time{"ann": {"billing": late, "search": old}},
		WantTimes: map[string]map[string]time.Time{"ann": {"billing": late, "search": late}},
	}, { // Test 1: A person and a subject only the source holds are added.
		Into: map[string]map[string]time.Time{"ann": {"billing": old}},
		From: map[string]map[string]time.Time{"ann": {"ledger": late}, "bob": {"search": old}},
		WantTimes: map[string]map[string]time.Time{
			"ann": {"billing": old, "ledger": late}, "bob": {"search": old},
		},
	}, { // Test 2: Nothing to merge leaves the destination as it was.
		Into:      map[string]map[string]time.Time{},
		WantTimes: map[string]map[string]time.Time{},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			mergeTopicTimes(test.Into, test.From)
			if diff := cmp.Diff(test.WantTimes, test.Into, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestTopicTimesJSON verifies dates are written as Unix seconds and read back
// as the same instants in UTC, and that malformed input is refused.
func TestTopicTimesJSON(t *testing.T) {
	t.Parallel()
	at := time.Date(2024, 3, 9, 14, 22, 31, 0, time.FixedZone("CST", -6*3600))
	tests := []struct {
		In       TopicTimes
		Raw      string
		WantRaw  string
		WantRead TopicTimes
		Want     error
	}{{ // Test 0: A date round-trips as Unix seconds.
		In:       TopicTimes{"billing": at},
		WantRaw:  `{"billing":1710015751}`,
		Raw:      `{"billing":1710015751}`,
		WantRead: TopicTimes{"billing": at.UTC()},
	}, { // Test 1: An empty set writes and reads as empty.
		In: TopicTimes{}, WantRaw: `{}`, Raw: `{}`, WantRead: TopicTimes{},
	}, { // Test 2: A JSON null reads as no dates.
		WantRaw: `{}`, Raw: `null`,
	}, { // Test 3: A date written as text is refused.
		WantRaw: `{}`, Raw: `{"billing":"2024-03-09"}`, Want: ErrTopicTimes,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(test.In)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if diff := cmp.Diff(test.WantRaw, string(raw)); diff != "" {
				t.Errorf("written mismatch (-want +got):\n%s", diff)
			}
			var got TopicTimes
			err = json.Unmarshal([]byte(test.Raw), &got)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Unmarshal error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantRead, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("read mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestGitDatesEachSubjectByItsOwnCommits checks a person's subjects are dated
// by the commits that touched them, not by their newest commit anywhere, and
// that a resumed read dates only what it read. Ann last touched billing three
// years ago and committed to search yesterday; billing must stay three years
// old, or her fresh search work would make her look current on billing.
func TestGitDatesEachSubjectByItsOwnCommits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	commit := func(rel string, when time.Time) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(when.String()), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := wt.Add(rel); err != nil {
			t.Fatalf("add: %v", err)
		}
		sig := &object.Signature{Name: "Ann Lee", Email: "ann@corp.com", When: when}
		if _, err := wt.Commit("change "+filepath.Dir(rel), &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	now := time.Now().Truncate(time.Second)
	billingAt := now.AddDate(-3, 0, 0)
	searchAt := now.AddDate(0, 0, -1)
	commit("billing/invoice.rb", billingAt.AddDate(0, 0, -7))
	commit("billing/invoice.rb", billingAt)
	commit("search/rank.rb", searchAt)

	first := NewGitHistory(GitOptions{Paths: []string{dir}, SinceDays: 2000})
	recs, err := first.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	ann := recordFor(t, recs, "ann@corp.com")
	if !ann.Time.Equal(searchAt) {
		t.Errorf("record time = %v, want her newest commit %v", ann.Time, searchAt)
	}
	want := map[string]time.Time{"billing": billingAt, "search": searchAt}
	for top, at := range want {
		if got := ann.TopicTimes[top]; !got.Equal(at) {
			t.Errorf("%s dated %v, want %v", top, got, at)
		}
	}

	// A resumed read sees only the new billing commit, so it dates billing and
	// nothing else; the index folds it onto the held dates.
	againAt := now.Add(-time.Hour)
	commit("billing/invoice.rb", againAt)
	resumed := NewGitHistory(GitOptions{
		Paths: []string{dir}, SinceDays: 2000, StopAt: map[string]string{dir: first.Marks()[dir]},
	})
	recs, err = resumed.Fetch(context.Background())
	if err != nil {
		t.Fatalf("resumed Fetch: %v", err)
	}
	ann = recordFor(t, recs, "ann@corp.com")
	if got := ann.TopicTimes["billing"]; !got.Equal(againAt) {
		t.Errorf("resumed billing dated %v, want %v", got, againAt)
	}
	if got, ok := ann.TopicTimes["search"]; ok {
		t.Errorf("resumed read dated search at %v, but read no search commit", got)
	}
}

// recordFor returns the person record for email, failing the test when
// there is none.
func recordFor(t *testing.T, recs []Record, email string) Record {
	t.Helper()
	for _, r := range recs {
		if r.Kind == KindPerson && r.Email == email {
			return r
		}
	}
	t.Fatalf("no record for %s in %d records", email, len(recs))
	return Record{}
}
