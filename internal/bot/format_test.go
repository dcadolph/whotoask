package bot

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dcadolph/whotoask/internal/model"
	"github.com/dcadolph/whotoask/internal/resolve"
)

// TestFormatEscapesMrkdwn verifies user-derived text cannot inject Slack
// broadcasts or mentions: the raw query, the summary, and person and channel
// fields are all escaped before they reach the mrkdwn reply.
func TestFormatEscapesMrkdwn(t *testing.T) {
	t.Parallel()

	// The no-match path still echoes the query and must escape it.
	noMatch := Format("<!channel> ping everyone", resolve.Answer{})
	if strings.Contains(noMatch, "<!channel>") {
		t.Errorf("no-match reply leaked a raw broadcast: %q", noMatch)
	}
	if !strings.Contains(noMatch, "&lt;!channel&gt;") {
		t.Errorf("no-match reply did not escape the query: %q", noMatch)
	}

	ans := resolve.Answer{
		Summary: "Talk to <!here>.",
		People: []model.Match{{
			Person: &model.Person{Name: "<!channel>", Title: "R&D <lead>"},
			Team:   &model.Team{Name: "A&B"},
		}},
		Channels: []model.ChannelMatch{{
			Channel: &model.Channel{Name: "<!everyone>", Topic: "1 > 0"},
		}},
	}
	got := Format("who knows <@U123>", ans)
	for _, raw := range []string{"<!channel>", "<!here>", "<!everyone>", "<@U123>", "<lead>"} {
		if strings.Contains(got, raw) {
			t.Errorf("formatted reply leaked raw mrkdwn %q:\n%s", raw, got)
		}
	}
	// A bare & from user data must become an entity, not stay a live control char.
	if strings.Contains(got, "A&B") || strings.Contains(got, "R&D") {
		t.Errorf("formatted reply left a raw ampersand:\n%s", got)
	}
	if !strings.Contains(got, "&lt;!channel&gt;") || !strings.Contains(got, "A&amp;B") {
		t.Errorf("formatted reply missing expected escapes:\n%s", got)
	}
}

// TestFormatTeams verifies the teams of record get their own section, with the
// org chart team beside a handle when one matches, and that an answer holding
// only teams is an answer rather than a miss.
func TestFormatTeams(t *testing.T) {
	t.Parallel()
	squad := model.Match{
		Person: &model.Person{ID: "codeowners:acme/legal-squad", Name: "@acme/legal-squad"},
		Team:   &model.Team{Name: "Legal"},
	}
	bare := model.Match{
		Person: &model.Person{ID: "codeowners:acme/default-maintainers", Name: "@acme/default-maintainers"},
	}
	person := model.Match{Person: &model.Person{Name: "Carla Counsel"}}
	tests := []struct {
		WantResult string
		Ans        resolve.Answer
	}{{ // Test 0: Teams alone.
		Ans:        resolve.Answer{Teams: []model.Match{squad, bare}},
		WantResult: "*Teams (owners of record)*\n• @acme/legal-squad (Legal)\n• @acme/default-maintainers",
	}, { // Test 1: Teams follow the people.
		Ans: resolve.Answer{People: []model.Match{person}, Teams: []model.Match{squad}},
		WantResult: "*People*\n• Carla Counsel\n\n" +
			"*Teams (owners of record)*\n• @acme/legal-squad (Legal)",
	}, { // Test 2: No teams, no section.
		Ans:        resolve.Answer{People: []model.Match{person}},
		WantResult: "*People*\n• Carla Counsel",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantResult+"\n\n_"+notFor+"_", Format("legal", test.Ans)); diff != "" {
				t.Errorf("Format mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFormatDeparted checks that deactivated people close the people list,
// marked, and that an answer holding only them is still an answer.
func TestFormatDeparted(t *testing.T) {
	t.Parallel()
	gone := model.Match{Person: &model.Person{Name: "Lena Gone", Account: model.AccountDeactivated}}
	here := model.Match{Person: &model.Person{Name: "Nora Here", Account: model.AccountActive}}
	tests := []struct {
		WantResult string
		Ans        resolve.Answer
	}{{ // Test 0: The departed follow the people in one list, marked.
		Ans:        resolve.Answer{People: []model.Match{here}, Departed: []model.Match{gone}},
		WantResult: "*People*\n• Nora Here\n• Lena Gone · _deactivated_",
	}, { // Test 1: Only the departed matched.
		Ans:        resolve.Answer{Departed: []model.Match{gone}},
		WantResult: "*People*\n• Lena Gone · _deactivated_",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantResult+"\n\n_"+notFor+"_", Format("ledgers", test.Ans)); diff != "" {
				t.Errorf("Format mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFormatCarriesReasons checks each row says why it matched, the quiet mark
// included, escaped, and with nothing beside a name that ranks the person.
func TestFormatCarriesReasons(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantResult string
		Ans        resolve.Answer
	}{{ // Test 0: A person's reasons, quiet mark included, sit under the name.
		Ans: resolve.Answer{People: []model.Match{{
			Person: &model.Person{Name: "Ada"}, Score: 9, Strength: 0.8,
			Reasons: []string{"ledger (topic, no recent activity on record)", "billing (title)"},
		}}},
		WantResult: "*People*\n• Ada\n" +
			"    ledger (topic, no recent activity on record) · billing (title)",
	}, { // Test 1: Channel and team reasons are escaped.
		Ans: resolve.Answer{
			Channels: []model.ChannelMatch{{
				Channel: &model.Channel{Name: "pay"}, Reasons: []string{"<!here> (mention)"},
			}},
			Teams: []model.Match{{
				Person: &model.Person{Name: "@acme/pay"}, Reasons: []string{"pay (owner)"},
			}},
		},
		WantResult: "*Channels*\n• #pay\n    &lt;!here&gt; (mention)\n\n" +
			"*Teams (owners of record)*\n• @acme/pay\n    pay (owner)",
	}, { // Test 2: A row with no reasons gets no reasons line.
		Ans:        resolve.Answer{People: []model.Match{{Person: &model.Person{Name: "Bo"}}}},
		WantResult: "*People*\n• Bo",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := Format("ledger", test.Ans)
			if diff := cmp.Diff(test.WantResult+"\n\n_"+notFor+"_", got); diff != "" {
				t.Errorf("Format mismatch (-want +got):\n%s", diff)
			}
			for _, banned := range []string{"%", "0.8", "strength"} {
				if strings.Contains(got, banned) {
					t.Errorf("reply ranks a person with %q:\n%s", banned, got)
				}
			}
		})
	}
}
