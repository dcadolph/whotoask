package resolve

import (
	"fmt"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dcadolph/whotoask/internal/connector"
	"github.com/dcadolph/whotoask/internal/index"
)

// TestRisk checks concentration scoring flags a sole-expert topic as critical,
// a shared topic as elevated, and that Departure lists what leaves with a person.
func TestRisk(t *testing.T) {
	t.Parallel()
	ix := index.New()
	ix.Build([]connector.Record{
		{Kind: connector.KindPerson, Name: "Ruth", Email: "ruth@x.com", Topics: []string{"auth", "secrets"}, Source: "t"},
		{Kind: connector.KindPerson, Name: "Ann", Email: "ann@x.com", Topics: []string{"billing"}, Source: "t"},
		{Kind: connector.KindPerson, Name: "Bob", Email: "bob@x.com", Topics: []string{"billing"}, Source: "t"},
	})
	ix.Canonicalize()

	byTopic := make(map[string]TopicRisk)
	for _, r := range Risk(ix, 0) {
		byTopic[r.Topic] = r
	}
	if a := byTopic["auth"]; a.Level != "critical" || a.BusFactor != 1 {
		t.Errorf("auth = %+v, want critical bus factor 1", a)
	}
	if b := byTopic["billing"]; b.Level != "elevated" || b.BusFactor != 2 {
		t.Errorf("billing = %+v, want elevated bus factor 2", b)
	}

	imp := Departure(ix, "ruth")
	if !slices.Equal(imp.Sole, []string{"auth", "secrets"}) {
		t.Errorf("Ruth sole topics = %v, want [auth secrets]", imp.Sole)
	}
	if none := Departure(ix, "nobodyxyz"); none.Person != "" {
		t.Errorf("unmatched query resolved to %q, want empty", none.Person)
	}
}

// TestSoleHolder checks who a subject rests on alone is read from who is on
// record, with closed accounts not counted as cover and a list cut at
// expertsShown unable to say.
func TestSoleHolder(t *testing.T) {
	t.Parallel()
	ann := RiskExpert{ID: "ann", Name: "Ann"}
	bob := RiskExpert{ID: "bob", Name: "Bob"}
	gone := RiskExpert{ID: "gone", Name: "Gone", Deactivated: true}
	tests := []struct {
		Experts    []RiskExpert
		WantHolder string
		WantOK     bool
	}{{ // Test 0: One person on record, with no lead needed.
		Experts: []RiskExpert{ann}, WantHolder: "ann", WantOK: true,
	}, { // Test 1: Two active people is not sole.
		Experts: []RiskExpert{ann, bob}, WantOK: false,
	}, { // Test 2: The only other person has a closed account.
		Experts: []RiskExpert{gone, ann}, WantHolder: "ann", WantOK: true,
	}, { // Test 3: A lone deactivated person still holds it alone.
		Experts: []RiskExpert{gone}, WantHolder: "gone", WantOK: true,
	}, { // Test 4: More than one person, all deactivated, nobody holds it alone.
		Experts: []RiskExpert{gone, {ID: "gone2", Deactivated: true}}, WantOK: false,
	}, { // Test 5: A full list may hide active people past the cut.
		Experts: []RiskExpert{ann, gone, gone, gone, gone}, WantOK: false,
	}, { // Test 6: Nobody on record.
		WantOK: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, ok := TopicRisk{Experts: test.Experts}.SoleHolder()
			if ok != test.WantOK || (ok && got.ID != test.WantHolder) {
				t.Errorf("SoleHolder = %q, %v; want %q, %v", got.ID, ok, test.WantHolder, test.WantOK)
			}
		})
	}
}

// TestDepartureReadsWhoIsOnRecord checks a subject named only by a source of
// record, which has no lead, still counts as resting on its one person, and
// that a share with no work behind it is marked declared.
func TestDepartureReadsWhoIsOnRecord(t *testing.T) {
	t.Parallel()
	ix := index.New()
	ix.Build([]connector.Record{
		{Kind: connector.KindPerson, Name: "Pam", Email: "pam@x.com", Topics: []string{"support"}, Source: "org-csv"},
		{Kind: connector.KindPerson, Name: "Dee", Email: "dee@x.com", Topics: []string{"billing"}, Source: "codeowners"},
		{Kind: connector.KindPerson, Name: "Sam", Email: "sam@x.com",
			Topics: []string{"billing", "billing", "billing", "billing"}, Source: "git"},
	})
	ix.Canonicalize()

	byTopic := make(map[string]TopicRisk)
	for _, r := range Risk(ix, 0) {
		byTopic[r.Topic] = r
	}
	if s := byTopic["support"]; s.Lead != "" || len(s.Experts) != 1 {
		t.Fatalf("support = %+v, want one person on record and no lead", s)
	}
	if got := Departure(ix, "pam@x.com").Sole; !slices.Equal(got, []string{"support"}) {
		t.Errorf("Pam sole = %v, want [support]", got)
	}
	declared := make(map[string]bool)
	for _, e := range byTopic["billing"].Experts {
		declared[e.Name] = e.Declared
	}
	if diff := cmp.Diff(map[string]bool{"Dee": true, "Sam": false}, declared); diff != "" {
		t.Errorf("declared mismatch (-want +got):\n%s", diff)
	}
}

// TestDepartureRefusesAmbiguousNames checks a query matching several people
// hands back every candidate instead of picking the lowest id, while a full
// name, an email, or a unique fragment still resolves.
func TestDepartureRefusesAmbiguousNames(t *testing.T) {
	t.Parallel()
	ix := index.New()
	ix.Build([]connector.Record{
		{Kind: connector.KindPerson, Name: "Stan Lewis", Email: "lewis@x.com", Topics: []string{"kafka"}, Source: "git"},
		{Kind: connector.KindPerson, Name: "Stan Gibb", Email: "gibb@x.com", Topics: []string{"search"}, Source: "git"},
	})
	ix.Canonicalize()
	tests := []struct {
		Query          string
		WantPerson     string
		WantCandidates int
	}{{ // Test 0: A shared first name is ambiguous.
		Query: "stan", WantCandidates: 2,
	}, { // Test 1: A full name names one person.
		Query: "Stan Lewis", WantPerson: "lewis@x.com",
	}, { // Test 2: An email names one person.
		Query: "gibb@x.com", WantPerson: "gibb@x.com",
	}, { // Test 3: A unique fragment names one person.
		Query: "gibb", WantPerson: "gibb@x.com",
	}, { // Test 4: No match names nobody and lists nobody.
		Query: "nobodyxyz",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			imp := Departure(ix, test.Query)
			if imp.Person != test.WantPerson || len(imp.Candidates) != test.WantCandidates {
				t.Errorf("Departure(%q) = person %q, %d candidates; want %q, %d",
					test.Query, imp.Person, len(imp.Candidates), test.WantPerson, test.WantCandidates)
			}
		})
	}
}
