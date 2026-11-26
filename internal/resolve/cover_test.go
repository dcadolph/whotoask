package resolve

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/whotoask/internal/index"
	"github.com/dcadolph/whotoask/internal/model"
)

// TestAlsoOnRecord checks the subject-side cover list: only subjects resting
// mainly on one person appear, the others on record are listed by name with no
// score, the person it rests on is never among them, and a team is never
// counted as a person.
func TestAlsoOnRecord(t *testing.T) {
	t.Parallel()
	ix := index.New()
	ix.Graph.Topics = map[model.ID]*model.Topic{
		"ledger":  {ID: "ledger", Name: "ledger", Curated: true},
		"billing": {ID: "billing", Name: "billing", Curated: true},
	}
	ix.Graph.People = map[model.ID]*model.Person{
		"ada@x.com": {ID: "ada@x.com", Name: "Ada", Topics: map[model.ID]float64{"ledger": 40}},
		"zed@x.com": {ID: "zed@x.com", Name: "Zed", Topics: map[model.ID]float64{"ledger": 1}},
		"bo@x.com": {
			ID: "bo@x.com", Name: "Bo", Account: model.AccountDeactivated,
			Topics: map[model.ID]float64{"ledger": 1},
		},
		"codeowners:acme/ledger": {
			ID: "codeowners:acme/ledger", Name: "@acme/ledger",
			Topics: map[model.ID]float64{"ledger": 1},
		},
		"cy@x.com": {ID: "cy@x.com", Name: "Cy", Topics: map[model.ID]float64{"billing": 5}},
		"di@x.com": {ID: "di@x.com", Name: "Di", Topics: map[model.ID]float64{"billing": 5}},
	}
	ix.Canonicalize()

	tests := []struct {
		WantResult []SubjectCover
		In         []TopicRisk
	}{{ // Test 0: A critical subject lists the others by name, holder left out.
		In: []TopicRisk{{
			Topic: "ledger", Level: "critical", Lead: "Ada", LeadID: "ada@x.com",
			Experts: []RiskExpert{{ID: "ada@x.com", Name: "Ada", Share: 0.9}},
		}},
		WantResult: []SubjectCover{{
			Subject: "ledger", RestsOn: "Ada", RestsOnID: "ada@x.com",
			AlsoOnRecord: []CoverPerson{
				{ID: "bo@x.com", Name: "Bo", Deactivated: true},
				{ID: "zed@x.com", Name: "Zed"},
			},
		}},
	}, { // Test 1: A subject spread across people is not listed.
		In: []TopicRisk{{
			Topic: "billing", Level: "ok",
			Experts: []RiskExpert{{ID: "cy@x.com", Name: "Cy"}, {ID: "di@x.com", Name: "Di"}},
		}},
		WantResult: []SubjectCover{},
	}, { // Test 2: Without a lead, the largest holder is who it rests on.
		In: []TopicRisk{{
			Topic: "ledger", Level: "critical",
			Experts: []RiskExpert{{ID: "ada@x.com", Name: "Ada"}},
		}},
		WantResult: []SubjectCover{{
			Subject: "ledger", RestsOn: "Ada", RestsOnID: "ada@x.com",
			AlsoOnRecord: []CoverPerson{
				{ID: "bo@x.com", Name: "Bo", Deactivated: true},
				{ID: "zed@x.com", Name: "Zed"},
			},
		}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := AlsoOnRecord(ix, test.In)
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
