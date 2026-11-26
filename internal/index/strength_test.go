package index

import (
	"fmt"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/whotoask/internal/connector"
)

// strengthIndex builds a small graph with distinct evidence classes: an
// explicit topic owner, a title match, and a person who only mentions the
// topic in free text.
func strengthIndex() *Index {
	ix := New()
	ix.Build([]connector.Record{{
		Kind: connector.KindPerson, Email: "owner@corp.com", Name: "Owner",
		Topics: []string{"kafka", "streaming"}, Source: "org-csv",
	}, {
		Kind: connector.KindPerson, Email: "titled@corp.com", Name: "Titled",
		Title: "Kafka Platform Engineer", Source: "org-csv",
	}, {
		Kind: connector.KindPerson, Email: "mention@corp.com", Name: "Mention",
		Text: "saw a kafka error in the logs once", Source: "slack",
	}, {
		Kind: connector.KindChannel, Name: "kafka", Title: "kafka questions",
		Source: "slack",
	}})
	return ix
}

func TestStrength(t *testing.T) {
	t.Parallel()
	ix := strengthIndex()
	tests := []struct {
		Query   string
		Person  string
		WantMin float64
		WantMax float64
	}{{ // Test 0: Full coverage on an explicit topic is certain.
		Query: "kafka streaming", Person: "owner@corp.com", WantMin: 1, WantMax: 1,
	}, { // Test 1: Half coverage on an explicit topic halves strength.
		Query: "kafka replication", Person: "owner@corp.com", WantMin: 0.5, WantMax: 0.5,
	}, { // Test 2: A title hit is slightly weaker than a topic hit.
		Query: "kafka", Person: "titled@corp.com", WantMin: 0.85, WantMax: 0.85,
	}, { // Test 3: A free-text mention is weak evidence.
		Query: "kafka", Person: "mention@corp.com", WantMin: 0.5, WantMax: 0.5,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			for _, m := range ix.Search(test.Query, 10) {
				if string(m.Person.ID) != test.Person {
					continue
				}
				if m.Strength < test.WantMin || m.Strength > test.WantMax {
					t.Errorf("strength = %.2f, want in [%.2f, %.2f]",
						m.Strength, test.WantMin, test.WantMax)
				}
				return
			}
			t.Fatalf("person %s not in results for %q", test.Person, test.Query)
		})
	}
}

func TestChannelStrength(t *testing.T) {
	t.Parallel()
	ix := strengthIndex()
	got := ix.SearchChannels("kafka", 3)
	if len(got) == 0 {
		t.Fatal("no channel matches")
	}
	if got[0].Channel.Name != "kafka" || got[0].Strength != 1 {
		t.Errorf("top channel = %s strength %.2f, want kafka at 1.00",
			got[0].Channel.Name, got[0].Strength)
	}
}

// correctedIndex builds a small graph for reading a misspelled question: one
// subject held as an explicit topic, as a title, and as a passing mention, a
// long subject that allows two edits, a subject reached through a synonym, and
// a channel named for two of them.
func correctedIndex() *Index {
	ix := New()
	ix.Build([]connector.Record{{
		Kind: connector.KindPerson, Email: "owner@corp.com", Name: "Owner",
		Topics: []string{"kafka", "elasticsearch", "vacation"}, Source: "org-csv",
	}, {
		Kind: connector.KindPerson, Email: "titled@corp.com", Name: "Titled",
		Title: "Kafka Platform Engineer", Source: "org-csv",
	}, {
		Kind: connector.KindPerson, Email: "mention@corp.com", Name: "Mention",
		Text: "saw a kafka error in the logs once", Source: "slack",
	}, {
		Kind: connector.KindChannel, Name: "kafka", Title: "kafka questions", Source: "slack",
	}, {
		Kind: connector.KindChannel, Name: "vacation", Title: "vacation requests", Source: "slack",
	}})
	return ix
}

// TestStrengthMarksDownACorrectedWord checks strength counts a corrected word
// for less than the word itself. Before this, a one-edit guess at a typo showed
// the same full bar as the exact word, so a wrong guess read as proof. A
// synonym is not a guess, so its ranking discount stays out of strength, and
// the correction never changes whether the match counts as declared.
func TestStrengthMarksDownACorrectedWord(t *testing.T) {
	t.Parallel()
	ix := correctedIndex()
	tests := []struct {
		Query        string
		Person       string
		Channel      string
		WantStrength float64
		WantDeclared bool
	}{{ // Test 0: One edit on an explicit topic.
		Query: "kafkaa", Person: "owner@corp.com", WantStrength: 0.7, WantDeclared: true,
	}, { // Test 1: One edit on a title.
		Query: "kafkaa", Person: "titled@corp.com", WantStrength: 0.85 * 0.7, WantDeclared: true,
	}, { // Test 2: One edit on a passing mention.
		Query: "kafkaa", Person: "mention@corp.com", WantStrength: 0.5 * 0.7,
	}, { // Test 3: The exact word keeps its full strength.
		Query: "kafka", Person: "owner@corp.com", WantStrength: 1, WantDeclared: true,
	}, { // Test 4: Two edits on a long word are marked down once, like one edit.
		Query: "elastcsearh", Person: "owner@corp.com", WantStrength: 0.7, WantDeclared: true,
	}, { // Test 5: An exact word beside a corrected one carries the match.
		Query: "kafkaa vacation", Person: "owner@corp.com", WantStrength: 1, WantDeclared: true,
	}, { // Test 6: A synonym is not a guess, so its discount stays out.
		Query: "time off", Person: "owner@corp.com", WantStrength: 1, WantDeclared: true,
	}, { // Test 7: One edit on a channel name.
		Query: "kafkaa", Channel: "kafka", WantStrength: 0.7, WantDeclared: true,
	}, { // Test 8: A channel's exact name keeps its full strength.
		Query: "kafka", Channel: "kafka", WantStrength: 1, WantDeclared: true,
	}, { // Test 9: A channel reached through a synonym is not marked down.
		Query: "time off", Channel: "vacation", WantStrength: 1, WantDeclared: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var strength float64
			var declared, found bool
			for _, m := range ix.Search(test.Query, 0) {
				if test.Person != "" && string(m.Person.ID) == test.Person {
					strength, declared, found = m.Strength, m.Declared, true
				}
			}
			for _, m := range ix.SearchChannels(test.Query, 0) {
				if test.Channel != "" && m.Channel.Name == test.Channel {
					strength, declared, found = m.Strength, m.Declared, true
				}
			}
			if !found {
				t.Fatalf("%q found neither %q nor %q", test.Query, test.Person, test.Channel)
			}
			approx := cmpopts.EquateApprox(0, 1e-9)
			if diff := cmp.Diff(test.WantStrength, strength, approx); diff != "" {
				t.Errorf("strength mismatch (-want +got):\n%s", diff)
			}
			if declared != test.WantDeclared {
				t.Errorf("declared = %v, want %v", declared, test.WantDeclared)
			}
		})
	}
}

// TestCapStrength checks a ranked list never reads as stronger further down
// than the order placed it, and that a row keeps its own strength while that is
// no higher than the row above.
func TestCapStrength(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         []float64
		WantResult []float64
	}{{ // Test 0: Nothing to cap.
		In: nil, WantResult: nil,
	}, { // Test 1: One row keeps its strength.
		In: []float64{0.5}, WantResult: []float64{0.5},
	}, { // Test 2: A falling list is left alone.
		In: []float64{1, 0.85, 0.5}, WantResult: []float64{1, 0.85, 0.5},
	}, { // Test 3: A stronger row below a weaker one is held to it.
		In: []float64{0.5, 0.85, 0.43}, WantResult: []float64{0.5, 0.5, 0.43},
	}, { // Test 4: The hold carries down a run of stronger rows.
		In: []float64{0.5, 1, 0.85, 0.25}, WantResult: []float64{0.5, 0.5, 0.5, 0.25},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := slices.Clone(test.In)
			CapStrength(got, func(f *float64) *float64 { return f })
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCoveredShare checks how much of a question a match is credited with: a
// word asked or its synonym covers that word, and a subject that only travels
// with the one asked earns the tie discount on the asked words of that subject.
func TestCoveredShare(t *testing.T) {
	t.Parallel()
	covers := map[string][]string{
		"billing": {"billing"}, "retries": {"retries"}, "invoices": {"billing"}, "kafka": nil,
	}
	asked := map[string]string{"invoices": "billing", "kafka": tieAskedPrefix + "billing-retries"}
	tests := []struct {
		Matched    []string
		WantResult float64
	}{{ // Test 0: Every word asked.
		Matched: []string{"billing", "retries"}, WantResult: 1,
	}, { // Test 1: Half the words asked.
		Matched: []string{"retries"}, WantResult: 0.5,
	}, { // Test 2: A synonym covers the word it stands for.
		Matched: []string{"invoices", "retries"}, WantResult: 1,
	}, { // Test 3: Only a tied subject earns the discount on every word it travels with.
		Matched: []string{"kafka"}, WantResult: tieExpansionPenalty,
	}, { // Test 4: A word asked outright is not marked down by a tie beside it.
		Matched: []string{"billing", "kafka"}, WantResult: (1 + tieExpansionPenalty) / 2,
	}, { // Test 5: Nothing matched covers nothing.
		Matched: nil, WantResult: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			matched := make(map[string]bool, len(test.Matched))
			for _, term := range test.Matched {
				matched[term] = true
			}
			got := coveredShare(matched, covers, asked, 2)
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateApprox(0, 1e-9)); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestReachStrength checks a person listed only for a subject that travels with
// the one asked carries a strength rather than zero, below the person who holds
// the subject asked. A zero read as a row nothing explains.
func TestReachStrength(t *testing.T) {
	t.Parallel()
	ix := tieFixture(t)
	tests := []struct {
		Person       string
		WantStrength float64
	}{{ // Test 0: The owner of the subject asked.
		Person: "billing@corp.com", WantStrength: 1,
	}, { // Test 1: The owner of the subject it travels with, discounted as a tie.
		Person: "kafka@corp.com", WantStrength: tieExpansionPenalty,
	}}
	got := ix.Search("billing retries", 0)
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			for _, m := range got {
				if string(m.Person.ID) != test.Person {
					continue
				}
				approx := cmpopts.EquateApprox(0, 1e-9)
				if diff := cmp.Diff(test.WantStrength, m.Strength, approx); diff != "" {
					t.Errorf("strength mismatch (-want +got):\n%s", diff)
				}
				return
			}
			t.Fatalf("%s not in results", test.Person)
		})
	}
}
