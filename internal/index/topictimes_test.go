package index

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/whotoask/internal/connector"
	"github.com/dcadolph/whotoask/internal/model"
)

// subjectIndex builds an index on the pinned clock over two people working on
// billing: Ann, whose billing work is three years old but who committed to
// search yesterday, and Bob, whose billing work is a month old.
func subjectIndex() *Index {
	ix := New()
	ix.now = func() time.Time { return evalNow }
	ix.Build([]connector.Record{{
		Email: "ann@corp.com", Name: "Ann", Source: "git",
		Topics: []string{"billing", "billing", "billing", "billing", "search"},
		Time:   evalNow.AddDate(0, 0, -1),
		TopicTimes: map[string]time.Time{
			"billing": evalNow.AddDate(-3, 0, 0), "search": evalNow.AddDate(0, 0, -1),
		},
	}, {
		Email: "bob@corp.com", Name: "Bob", Source: "git",
		Topics: []string{"billing"}, Time: evalNow.AddDate(0, -1, 0),
		TopicTimes: map[string]time.Time{"billing": evalNow.AddDate(0, -1, 0)},
	}, {
		Email: "cy@corp.com", Name: "Cy", Source: "git",
		Topics: []string{"billing"}, Time: evalNow.AddDate(-1, 0, 0),
	}})
	return ix
}

// TestDecayIsPerSubject checks each subject's weight decays from the newest
// activity on that subject. Ann's fresh search commit must not restore full
// weight to her three-year-old billing work, and a record dating no subject
// still decays by its own time.
func TestDecayIsPerSubject(t *testing.T) {
	t.Parallel()
	ix := subjectIndex()
	tests := []struct {
		Person     model.ID
		Topic      model.ID
		WantWeight float64
	}{{ // Test 0: Old billing work decays from its own date.
		Person: "ann@corp.com", Topic: "billing",
		WantWeight: 4 * weightTopic * ix.decay(evalNow.AddDate(-3, 0, 0)),
	}, { // Test 1: Fresh search work keeps nearly full weight.
		Person: "ann@corp.com", Topic: "search",
		WantWeight: weightTopic * ix.decay(evalNow.AddDate(0, 0, -1)),
	}, { // Test 2: A record dating no subject decays by its own time.
		Person: "cy@corp.com", Topic: "billing",
		WantWeight: weightTopic * ix.decay(evalNow.AddDate(-1, 0, 0)),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := ix.Graph.People[test.Person].Topics[test.Topic]
			if math.Abs(got-test.WantWeight) > 1e-9 {
				t.Errorf("weight = %.6f, want %.6f", got, test.WantWeight)
			}
		})
	}
	got := ix.Search("billing", 3)
	if len(got) == 0 || got[0].Person.ID != "bob@corp.com" {
		t.Errorf("billing top = %v, want bob, whose billing work is the recent one", topID(got))
	}
}

// TestRecentSubjectsAgeOut checks a subject a source called recent stops being
// recent once its newest activity is older than the recent window, while a
// subject still inside it, or one the source never dated, stays.
func TestRecentSubjectsAgeOut(t *testing.T) {
	t.Parallel()
	ix := New()
	ix.now = func() time.Time { return evalNow }
	ix.Build([]connector.Record{{
		Email: "ann@corp.com", Name: "Ann", Source: "git",
		Topics:       []string{"billing", "search", "ledger"},
		RecentTopics: []string{"billing", "search", "ledger"},
		Time:         evalNow.AddDate(0, 0, -10),
		TopicTimes: map[string]time.Time{
			"billing": evalNow.Add(-connector.RecentWindow - 24*time.Hour),
			"search":  evalNow.AddDate(0, 0, -10),
		},
	}})
	got := ix.Graph.People["ann@corp.com"].Recent
	for _, top := range []model.ID{"search", "ledger"} {
		if got[top] == 0 {
			t.Errorf("%s dropped from recent work, want it kept", top)
		}
	}
	if w, ok := got["billing"]; ok {
		t.Errorf("billing still recent at %.3f, but its newest work is past the window", w)
	}
}

// TestFoldTopicTimes checks folding keeps each subject's later date and dates
// a subject held without one by its own record's time.
func TestFoldTopicTimes(t *testing.T) {
	t.Parallel()
	old := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		Base      connector.Record
		Add       connector.Record
		WantTimes connector.TopicTimes
	}{{ // Test 0: Neither side dates a subject, so the fold dates none.
		Base: connector.Record{Topics: []string{"billing"}, Time: old},
		Add:  connector.Record{Topics: []string{"search"}, Time: late},
	}, { // Test 1: A held record with no dates keeps its subjects at its own time.
		Base: connector.Record{Topics: []string{"billing"}, WeakTopics: []string{"invoice"}, Time: old},
		Add: connector.Record{
			Topics: []string{"search"}, Time: late,
			TopicTimes: map[string]time.Time{"search": late},
		},
		WantTimes: connector.TopicTimes{"billing": old, "invoice": old, "search": late},
	}, { // Test 2: Both sides dated keep the later date per subject.
		Base: connector.Record{
			Topics: []string{"billing", "search"}, Time: mid,
			TopicTimes: map[string]time.Time{"billing": old, "search": mid},
		},
		Add: connector.Record{
			Topics: []string{"billing"}, Time: late,
			TopicTimes: map[string]time.Time{"billing": late},
		},
		WantTimes: connector.TopicTimes{"billing": late, "search": mid},
	}, { // Test 3: A recent subject the incoming record left undated takes its time.
		Base: connector.Record{
			Topics: []string{"billing"}, Time: old,
			TopicTimes: map[string]time.Time{"billing": old},
		},
		Add:       connector.Record{RecentTopics: []string{"ledger"}, Time: mid},
		WantTimes: connector.TopicTimes{"billing": old, "ledger": mid},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := foldRecord(test.Base, test.Add).TopicTimes
			if diff := cmp.Diff(test.WantTimes, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestIncrementalKeepsSubjectDates checks folding a later read onto a held
// one, with decay on, answers the same as one full read of both, and that the
// dates survive a save and a reload. An increment that touched only search
// must not freshen billing.
func TestIncrementalKeepsSubjectDates(t *testing.T) {
	t.Parallel()
	t1 := evalNow.AddDate(-3, 0, 0)
	t2 := evalNow.AddDate(0, 0, -1)
	held := []connector.Record{{
		Source: "git", Email: "ann@corp.com", Name: "Ann",
		Topics: []string{"billing", "billing"}, Time: t1,
		TopicTimes: map[string]time.Time{"billing": t1},
	}, {
		Source: "git", Email: "bob@corp.com", Name: "Bob",
		Topics: []string{"billing"}, Time: evalNow.AddDate(0, -2, 0),
		TopicTimes: map[string]time.Time{"billing": evalNow.AddDate(0, -2, 0)},
	}}
	delta := []connector.Record{{
		Source: "git", Email: "ann@corp.com", Name: "Ann",
		Topics: []string{"search"}, Time: t2,
		TopicTimes: map[string]time.Time{"search": t2},
	}}
	full := []connector.Record{{
		Source: "git", Email: "ann@corp.com", Name: "Ann",
		Topics: []string{"billing", "billing", "search"}, Time: t2,
		TopicTimes: map[string]time.Time{"billing": t1, "search": t2},
	}, held[1]}

	ixFull := New()
	ixFull.now = func() time.Time { return evalNow }
	ixFull.Build(full)

	ixInc := New()
	ixInc.now = func() time.Time { return evalNow }
	ixInc.Build(held)
	ixInc.MergeIncremental(delta)

	path := filepath.Join(t.TempDir(), "index.json")
	if err := ixInc.Save(path, WithNow(func() time.Time { return evalNow })); err != nil {
		t.Fatalf("Save: %v", err)
	}
	ixLoaded, err := Load(path, WithNow(func() time.Time { return evalNow }))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := ixLoaded.LoadSources(path, WithNow(func() time.Time { return evalNow })); err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	ixLoaded.rebuild()

	for _, q := range []string{"billing", "search"} {
		want := rows(ixFull.Search(q, 5))
		if diff := cmp.Diff(want, rows(ixInc.Search(q, 5))); diff != "" {
			t.Errorf("%q incremental differs from full (-full +incremental):\n%s", q, diff)
		}
		if diff := cmp.Diff(want, rows(ixLoaded.Search(q, 5))); diff != "" {
			t.Errorf("%q reloaded differs from full (-full +reloaded):\n%s", q, diff)
		}
	}
	if got := ixInc.Search("billing", 1); len(got) == 0 || got[0].Person.ID != "bob@corp.com" {
		t.Errorf("billing top = %v, want bob once ann's increment touched only search", topID(got))
	}
}

// topID names the top match for an error message, or "none".
func topID(matches []model.Match) string {
	if len(matches) == 0 {
		return "none"
	}
	return string(matches[0].Person.ID)
}
