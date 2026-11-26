package bot

import (
	"slices"
	"strings"

	"github.com/dcadolph/whotoask/internal/resolve"
)

// notFor closes every answer that names people. It is the sentence every other
// surface carries, so a reply quoted into a thread keeps it.
const notFor = "WhoToAsk describes the record, not the person. " +
	"Its findings must not be used for performance reviews, layoffs, " +
	"or any other employment decision."

// Format renders an answer as Slack mrkdwn. Every value drawn from the query or
// the graph is escaped, so text like <!channel> or <@Uxxx> renders as literal
// characters rather than a workspace-wide broadcast or a mention. Each row
// carries the reasons it matched, which is where a subject somebody has stopped
// working on says so, and nothing beside a name ranks the person: no strength,
// percent, bar, or position number.
func Format(query string, ans resolve.Answer) string {
	if len(ans.People) == 0 && len(ans.Departed) == 0 && len(ans.Channels) == 0 && len(ans.Teams) == 0 {
		return "No matches for *" + escapeMrkdwn(query) + "*. Try different words."
	}

	var b strings.Builder
	if ans.Summary != "" {
		b.WriteString(escapeMrkdwn(ans.Summary))
		b.WriteString("\n\n")
	}

	// The departed close the people list, marked, so the ranking reads as one
	// list with them at the bottom.
	if people := slices.Concat(ans.People, ans.Departed); len(people) > 0 {
		b.WriteString("*People*\n")
		for _, m := range people {
			name := m.Person.Name
			if name == "" {
				name = m.Person.Email
			}
			line := "• " + escapeMrkdwn(name)
			if m.Person.Title != "" {
				line += " - " + escapeMrkdwn(m.Person.Title)
			}
			if m.Team != nil && m.Team.Name != "" {
				line += " (" + escapeMrkdwn(m.Team.Name) + ")"
			}
			if m.Person.Deactivated() {
				line += " · _deactivated_"
			}
			b.WriteString(line)
			b.WriteString("\n")
			writeReasons(&b, m.Reasons)
		}
	}

	if len(ans.Channels) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("*Channels*\n")
		for _, c := range ans.Channels {
			line := "• #" + escapeMrkdwn(c.Channel.Name)
			if c.Channel.Topic != "" {
				line += " - " + escapeMrkdwn(c.Channel.Topic)
			}
			b.WriteString(line)
			b.WriteString("\n")
			writeReasons(&b, c.Reasons)
		}
	}

	if len(ans.Teams) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("*Teams (owners of record)*\n")
		for _, m := range ans.Teams {
			line := "• " + escapeMrkdwn(m.Person.Name)
			if m.Team != nil && m.Team.Name != "" {
				line += " (" + escapeMrkdwn(m.Team.Name) + ")"
			}
			b.WriteString(line)
			b.WriteString("\n")
			writeReasons(&b, m.Reasons)
		}
	}

	b.WriteString("\n_" + notFor + "_")
	return b.String()
}

// writeReasons writes the reasons a row matched on an indented line beneath
// it, or nothing when the row has none.
func writeReasons(b *strings.Builder, reasons []string) {
	if len(reasons) == 0 {
		return
	}
	escaped := make([]string, 0, len(reasons))
	for _, r := range reasons {
		escaped = append(escaped, escapeMrkdwn(r))
	}
	b.WriteString("    " + strings.Join(escaped, " · ") + "\n")
}

// escapeMrkdwn escapes the three characters Slack treats as mrkdwn control
// characters. The ampersand is replaced first, so the entities the other two
// introduce are not escaped again.
func escapeMrkdwn(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
