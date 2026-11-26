package resolve

import (
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/dcadolph/whotoask/internal/index"
	"github.com/dcadolph/whotoask/internal/model"
	"github.com/dcadolph/whotoask/internal/util"
)

// Directory is the browsable inventory of everything indexed, for the web
// UI's directory views. Build it once per process; the graph does not change
// while serving.
type Directory struct {
	// People lists everyone, sorted by display name.
	People []DirectoryPerson `json:"people"`
	// Channels lists every channel, sorted by name.
	Channels []DirectoryChannel `json:"channels"`
	// Teams lists every team of record with its size, and every CODEOWNERS
	// group marked as one, sorted by name.
	Teams []DirectoryTeam `json:"teams"`
	// Topics lists every subject people are on record for, folded to one per
	// group, with how many people carry it, most first.
	Topics []DirectoryTopic `json:"topics"`
}

// DirectoryPerson is one row of the people directory.
type DirectoryPerson struct {
	// ID is the person's canonical identifier.
	ID string `json:"id"`
	// Name is the display name, falling back to the identifier.
	Name string `json:"name"`
	// Email is the person's work email.
	Email string `json:"email,omitempty"`
	// Title is the person's job title.
	Title string `json:"title,omitempty"`
	// Team is the person's team name.
	Team string `json:"team,omitempty"`
	// Org is the person's organization name.
	Org string `json:"org,omitempty"`
	// Deactivated marks a person whose every known account is deactivated.
	Deactivated bool `json:"deactivated,omitempty"`
	// Topics are the person's strongest expertise areas, strongest first.
	Topics []string `json:"topics,omitempty"`
	// Alone are the subjects this person is the only holder of. Those leave the
	// organization the day they do, which is the finding a reporting chart
	// cannot show and the reason to draw one at all.
	Alone []string `json:"alone,omitempty"`
	// Quiet are subjects they still hold and have stopped working on. Knowledge
	// that has gone still is a sharper warning than knowledge held by few.
	Quiet []string `json:"quiet,omitempty"`
	// ManagerID is the person's manager's canonical id, empty for a root.
	ManagerID string `json:"managerId,omitempty"`
}

// DirectoryChannel is one row of the channel directory.
type DirectoryChannel struct {
	// Name is the channel name without a leading symbol.
	Name string `json:"name"`
	// Topic is the channel's stated topic.
	Topic string `json:"topic,omitempty"`
	// Members is how many people are active in the channel.
	Members int `json:"members"`
}

// DirectoryTeam is one row of the team directory.
type DirectoryTeam struct {
	// Name is the team name.
	Name string `json:"name"`
	// Org is the team's organization name.
	Org string `json:"org,omitempty"`
	// People is how many people the team has. It is zero for a Group, whose
	// membership is not on record.
	People int `json:"people"`
	// Group marks a CODEOWNERS group: declared as an owner, with no membership
	// on record, so it is not a team of record and its People is unknown rather
	// than zero.
	Group bool `json:"group,omitempty"`
	// Topics are what the team works on most, so the row says something about
	// the team rather than only counting it.
	Topics []string `json:"topics,omitempty"`
}

// DirectoryTopic is one row of the topic directory.
type DirectoryTopic struct {
	// Name is the topic slug, which doubles as its display name.
	Name string `json:"name"`
	// People is how many people carry the topic.
	People int `json:"people"`
	// Lead is the display name of the subject's lead on record, the same person
	// risk and ownership name, so a row says who to ask without another click.
	// Empty when nobody has done work on it beyond being assigned it.
	Lead string `json:"lead,omitempty"`
	// Active is how many of those people have worked on it lately. A subject
	// several people know and nobody still touches is the interesting case, and
	// a bare head count hides it.
	Active int `json:"active"`
}

// BuildDirectory assembles the directory from the index graph.
func BuildDirectory(ix *index.Index) Directory {
	g := ix.Graph
	var d Directory

	teamSize := make(map[model.ID]int)
	topicSize := make(map[model.ID]int)
	// What each team's people work on, summed, so a team row can say what the
	// team is about instead of only how many are in it.
	teamWeight := make(map[model.ID]map[model.ID]float64)
	var squads []DirectoryTeam
	// People run in ID order: a team's topic weights sum one share per member,
	// and in map order those sums differ in their last digits between runs.
	for _, id := range slices.Sorted(maps.Keys(g.People)) {
		p := g.People[id]
		// A group named as a CODEOWNERS owner is a team, not a person: it
		// belongs on the teams list with the areas it was declared over, and
		// its membership is unknown here rather than zero-sized.
		if util.NamesATeam(string(p.ID)) {
			name := p.Name
			if name == "" {
				name = string(p.ID)
			}
			squads = append(squads, DirectoryTeam{
				Name: name, Group: true, Topics: salientTopics(ix, p.Topics, 4),
			})
			continue
		}
		row := DirectoryPerson{
			ID:          string(p.ID),
			Name:        p.Name,
			Email:       p.Email,
			Title:       p.Title,
			Deactivated: p.Deactivated(),
			Topics:      salientTopics(ix, p.Topics, 6),
			ManagerID:   string(p.ManagerID),
		}
		if row.Name == "" {
			row.Name = row.ID
		}
		if t := g.Teams[p.TeamID]; t != nil {
			row.Team = t.Name
			teamSize[p.TeamID]++
			into := teamWeight[p.TeamID]
			if into == nil {
				into = make(map[model.ID]float64)
				teamWeight[p.TeamID] = into
			}
			for tid, w := range p.Topics {
				if g.Topics[tid].Salient() {
					into[tid] += w
				}
			}
		}
		if o := g.Orgs[p.OrgID]; o != nil {
			row.Org = o.Name
		}
		for tid := range p.Topics {
			topicSize[tid]++
		}
		d.People = append(d.People, row)
	}
	// Who is the only holder of something, and what has gone still, filled once
	// every person has been counted. Both are per-person readings of the same
	// counts the topic list is built from.
	byID := make(map[string]*model.Person, len(g.People))
	for _, p := range g.People {
		byID[string(p.ID)] = p
	}
	for i := range d.People {
		p := byID[d.People[i].ID]
		if p == nil {
			continue
		}
		for _, name := range d.People[i].Topics {
			tid := model.ID(name)
			if topicSize[tid] == 1 {
				d.People[i].Alone = append(d.People[i].Alone, name)
			}
			if hasMovedOn(ix, p.ID, tid) {
				d.People[i].Quiet = append(d.People[i].Quiet, name)
			}
		}
	}
	sort.Slice(d.People, func(i, j int) bool {
		if li, lj := strings.ToLower(d.People[i].Name), strings.ToLower(d.People[j].Name); li != lj {
			return li < lj
		}
		// Two people can share a display name, so the unique id keeps the order
		// stable between runs rather than following random map iteration.
		return d.People[i].ID < d.People[j].ID
	})

	for _, ch := range g.Channels {
		d.Channels = append(d.Channels, DirectoryChannel{
			Name: ch.Name, Topic: ch.Topic, Members: len(ch.Members),
		})
	}
	sort.Slice(d.Channels, func(i, j int) bool {
		if li, lj := strings.ToLower(d.Channels[i].Name), strings.ToLower(d.Channels[j].Name); li != lj {
			return li < lj
		}
		return d.Channels[i].Name < d.Channels[j].Name
	})

	for tid, t := range g.Teams {
		row := DirectoryTeam{
			Name: t.Name, People: teamSize[tid],
			Topics: topTopics(teamWeight[tid], 4),
		}
		if o := g.Orgs[t.OrgID]; o != nil {
			row.Org = o.Name
		}
		d.Teams = append(d.Teams, row)
	}
	d.Teams = append(d.Teams, squads...)
	sort.Slice(d.Teams, func(i, j int) bool {
		if li, lj := strings.ToLower(d.Teams[i].Name), strings.ToLower(d.Teams[j].Name); li != lj {
			return li < lj
		}
		return d.Teams[i].Org < d.Teams[j].Org
	})

	// The browsable list is the subjects the organization actually has, folded
	// to one entry each. Topic text is mined from prose as well as declared, so
	// the raw set is mostly words like "issue" and "runbook" alongside the same
	// subject under several names: useful as ranking signal, useless as a list
	// of what a company knows. Counting people per group rather than summing
	// the parts keeps somebody who holds both "cdn" and "cdn-caching" counted
	// once. If nothing survives, the unfiltered set is listed rather than an
	// empty page.
	groups := topicGroups(ix)
	holders := subjectHolders(ix, groups)
	lead := leads(ix)
	for name, people := range holders {
		row := DirectoryTopic{Name: name, People: len(people)}
		if who := lead[model.ID(name)]; who != "" {
			row.Lead = personName(ix, who)
		}
		for id := range people {
			p := g.People[id]
			if p == nil || p.Deactivated() {
				continue
			}
			for tid, w := range p.Recent {
				if w > 0 && (groups[string(tid)] == name || string(tid) == name) {
					row.Active++
					break
				}
			}
		}
		d.Topics = append(d.Topics, row)
	}
	if len(d.Topics) == 0 {
		for tid, count := range topicSize {
			d.Topics = append(d.Topics, DirectoryTopic{Name: string(tid), People: count})
		}
	}
	sort.Slice(d.Topics, func(i, j int) bool {
		if d.Topics[i].People != d.Topics[j].People {
			return d.Topics[i].People > d.Topics[j].People
		}
		return d.Topics[i].Name < d.Topics[j].Name
	})
	return d
}

// subjectHolders maps each subject the organization has, folded to one name per
// group, to the people on record for it. A team of record is not a person and
// is left out, so a count of holders is a count of people.
func subjectHolders(ix *index.Index, groups map[string]string) map[string]map[model.ID]bool {
	g := ix.Graph
	holders := make(map[string]map[model.ID]bool)
	for id, p := range g.People {
		if id.IsTeam() {
			continue
		}
		for tid := range p.Topics {
			if !g.Topics[tid].Salient() {
				continue
			}
			name := groups[string(tid)]
			if name == "" {
				name = string(tid)
			}
			if holders[name] == nil {
				holders[name] = make(map[model.ID]bool)
			}
			holders[name][id] = true
		}
	}
	return holders
}

// SubjectCount returns how many subjects the organization has: the salient
// topics held by at least one person, folded to one per group. It is the count
// the directory's topic list and the risk view are built over, so a status line
// and a browsable list name the same number.
func SubjectCount(ix *index.Index) int {
	return len(subjectHolders(ix, topicGroups(ix)))
}
