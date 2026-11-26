package resolve

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/whotoask/internal/connector"
	"github.com/dcadolph/whotoask/internal/index"
	"github.com/dcadolph/whotoask/internal/model"
)

// TestReachable verifies a person counts as reachable only through a link the
// Mac app opens, and never through a Slack link once deactivated.
func TestReachable(t *testing.T) {
	t.Parallel()
	slack := "slack://user?team=T0&id=U1"
	tests := []struct {
		Links       []string
		Deactivated bool
		WantResult  bool
	}{{  // Test 0: No links.
	}, { // Test 1: An active person's Slack link.
		Links: []string{slack}, WantResult: true,
	}, { // Test 2: A deactivated person's Slack link.
		Links: []string{slack}, Deactivated: true,
	}, { // Test 3: The scheme's case does not matter.
		Links: []string{"SLACK://user?team=T0&id=U1"}, WantResult: true,
	}, { // Test 4: A deactivated person's Slack link in any case.
		Links: []string{"Slack://user?team=T0&id=U1"}, Deactivated: true,
	}, { // Test 5: A deactivated person's web link still reaches somebody.
		Links: []string{slack, "https://corp.example.com/u/1"}, Deactivated: true, WantResult: true,
	}, { // Test 6: Mail.
		Links: []string{"mailto:jane@corp.com"}, WantResult: true,
	}, { // Test 7: Plain http.
		Links: []string{"http://corp.example.com/u/1"}, WantResult: true,
	}, { // Test 8: Schemes the app does not open.
		Links: []string{"ftp://corp.example.com/u/1", "file:///etc/hosts"},
	}, { // Test 9: Links that do not parse.
		Links: []string{"://bad", "%zz"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := reachable(test.Links, test.Deactivated); got != test.WantResult {
				t.Errorf("reachable(%q, %v) = %v", test.Links, test.Deactivated, got)
			}
		})
	}
}

// TestPersonViewReachable verifies the answer JSON marks a person with a Slack
// link reachable and leaves the key out for a person with no link and for a
// deactivated person whose only link is Slack.
func TestPersonViewReachable(t *testing.T) {
	t.Parallel()
	ix := index.New()
	ix.Build([]connector.Record{
		{Kind: connector.KindPerson, Source: "slack", PersonID: "slack:U1", Name: "Jane Roe",
			Email: "jane@corp.com", Link: "slack://user?team=T0&id=U1", Account: model.AccountActive},
		{Kind: connector.KindPerson, Source: "git", Name: "Ada Lane", Email: "ada@corp.com"},
		{Kind: connector.KindPerson, Source: "slack", PersonID: "slack:U2", Name: "Lena Gone",
			Email: "lena@corp.com", Link: "slack://user?team=T0&id=U2", Account: model.AccountDeactivated},
	})
	ix.Canonicalize()
	match := func(id string) model.Match {
		p := ix.Graph.People[model.ID(id)]
		if p == nil {
			t.Fatalf("fixture: %s not found", id)
		}
		return model.Match{Person: p, Reasons: []string{"billing (topic)"}}
	}
	ans := Answer{
		ix:       ix,
		People:   []model.Match{match("jane@corp.com"), match("ada@corp.com")},
		Departed: []model.Match{match("lena@corp.com")},
	}
	raw, err := json.Marshal(ans.View("billing"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		People   []map[string]any `json:"people"`
		Departed []map[string]any `json:"departed"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.People) != 2 || len(got.Departed) != 1 {
		t.Fatalf("want 2 people and 1 departed: %s", raw)
	}
	if got.People[0]["reachable"] != true {
		t.Errorf("Jane, with a Slack link, is not reachable: %v", got.People[0])
	}
	if _, ok := got.People[1]["reachable"]; ok {
		t.Errorf("Ada, with no link, carries the key: %v", got.People[1])
	}
	if _, ok := got.Departed[0]["reachable"]; ok || got.Departed[0]["deactivated"] != true {
		t.Errorf("Lena, gone with only a Slack link, is marked wrong: %v", got.Departed[0])
	}
}

// TestProfileViewLinks verifies the profile JSON lists a person's deep links
// and leaves the field out for a person with none.
func TestProfileViewLinks(t *testing.T) {
	t.Parallel()
	ix := index.New()
	ix.Build([]connector.Record{
		{Kind: connector.KindPerson, Source: "slack", PersonID: "slack:U1", Name: "Jane Roe",
			Email: "jane@corp.com", Link: "slack://user?team=T0&id=U1"},
		{Kind: connector.KindPerson, Source: "git", Name: "Ada Lane", Email: "ada@corp.com"},
	})
	ix.Canonicalize()
	tests := []struct {
		ID        string
		WantLinks []string
	}{{ // Test 0: Slack person.
		ID: "jane@corp.com", WantLinks: []string{"slack://user?team=T0&id=U1"},
	}, { // Test 1: Person with no Slack identity.
		ID: "ada@corp.com",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			profile, ok := ix.Profile(model.ID(test.ID))
			if !ok {
				t.Fatalf("profile %s not found", test.ID)
			}
			raw, err := json.Marshal(ProfileView(ix, profile))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got struct {
				Links []string `json:"links"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if diff := cmp.Diff(test.WantLinks, got.Links); diff != "" {
				t.Errorf("links mismatch (-want +got):\n%s", diff)
			}
			if test.WantLinks == nil && strings.Contains(string(raw), `"links"`) {
				t.Errorf("profile with no links still has the field: %s", raw)
			}
		})
	}
}

// TestViewStrengthFollowsOrder verifies every list in the view reads no
// stronger further down than its order, whatever resolver set that order, and
// that a row with no reason is left out rather than listed unexplained.
func TestViewStrengthFollowsOrder(t *testing.T) {
	t.Parallel()
	person := func(id string, strength float64, reasons ...string) model.Match {
		return model.Match{Person: &model.Person{ID: model.ID(id), Name: id}, Strength: strength, Reasons: reasons}
	}
	channel := func(name string, strength float64, reasons ...string) model.ChannelMatch {
		return model.ChannelMatch{
			Channel: &model.Channel{ID: model.ID(name), Name: name}, Strength: strength, Reasons: reasons,
		}
	}
	tests := []struct {
		In            Answer
		WantPeople    []float64
		WantDeparted  []float64
		WantChannels  []float64
		WantTeams     []float64
		WantPeopleIDs []string
	}{{ // Test 0: A named person first, then a stronger subject match held to it.
		In: Answer{People: []model.Match{
			person("named", 0.9, "name"), person("owner", 1, "billing (topic)"),
			person("mention", 0.5, "billing (mention)"),
		}},
		WantPeople: []float64{0.9, 0.9, 0.5}, WantPeopleIDs: []string{"named", "owner", "mention"},
	}, { // Test 1: A row with no reason is left out, and the rest keep their order.
		In: Answer{People: []model.Match{
			person("owner", 1, "billing (topic)"), person("unexplained", 0), person("mention", 0.5, "billing (mention)"),
		}},
		WantPeople: []float64{1, 0.5}, WantPeopleIDs: []string{"owner", "mention"},
	}, { // Test 2: Departed, channels, and teams are each held to their own order.
		In: Answer{
			Departed: []model.Match{person("gone", 0.5, "billing (topic)"), person("gone2", 1, "billing (topic)")},
			Channels: []model.ChannelMatch{
				channel("payments", 0.5, "billing (topic)"), channel("billing", 1, "billing (name)"),
				channel("quiet", 0.2),
			},
			Teams: []model.Match{person("team:a", 0.7, "billing (topic)"), person("team:b", 1, "billing (topic)")},
		},
		WantPeople: []float64{}, WantDeparted: []float64{0.5, 0.5}, WantChannels: []float64{0.5, 0.5},
		WantTeams: []float64{0.7, 0.7}, WantPeopleIDs: []string{},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			view := test.In.View("billing")
			strengths := func(n int, at func(int) float64) []float64 {
				out := make([]float64, n)
				for i := range out {
					out[i] = at(i)
				}
				return out
			}
			ids := make([]string, len(view.People))
			for i, p := range view.People {
				ids[i] = p.ID
			}
			empty := cmpopts.EquateEmpty()
			if diff := cmp.Diff(test.WantPeopleIDs, ids, empty); diff != "" {
				t.Errorf("people mismatch (-want +got):\n%s", diff)
			}
			got := strengths(len(view.People), func(i int) float64 { return view.People[i].Strength })
			if diff := cmp.Diff(test.WantPeople, got, empty); diff != "" {
				t.Errorf("people strength mismatch (-want +got):\n%s", diff)
			}
			got = strengths(len(view.Departed), func(i int) float64 { return view.Departed[i].Strength })
			if diff := cmp.Diff(test.WantDeparted, got, empty); diff != "" {
				t.Errorf("departed strength mismatch (-want +got):\n%s", diff)
			}
			got = strengths(len(view.Channels), func(i int) float64 { return view.Channels[i].Strength })
			if diff := cmp.Diff(test.WantChannels, got, empty); diff != "" {
				t.Errorf("channel strength mismatch (-want +got):\n%s", diff)
			}
			got = strengths(len(view.Teams), func(i int) float64 { return view.Teams[i].Strength })
			if diff := cmp.Diff(test.WantTeams, got, empty); diff != "" {
				t.Errorf("team strength mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
