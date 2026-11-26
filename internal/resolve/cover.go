package resolve

import (
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/dcadolph/whotoask/internal/index"
)

// SubjectCover is one subject that rests mainly on one person, with everyone
// else the record also shows on it. It describes the subject, not the people:
// the others are listed by name, carry no score or order of preference, and
// none of them is designated to take anything over.
type SubjectCover struct {
	// Subject is the subject, under the name the risk findings use.
	Subject string `json:"subject"`
	// RestsOn is the display name of the person the subject rests mainly on.
	RestsOn string `json:"restsOn"`
	// RestsOnID is that person's canonical identifier.
	RestsOnID string `json:"restsOnId"`
	// AlsoOnRecord are the other people on record for the subject, sorted by
	// name. Empty when the record shows nobody else on it at all.
	AlsoOnRecord []CoverPerson `json:"alsoOnRecord"`
}

// CoverPerson is one person also on record for a subject.
type CoverPerson struct {
	// ID is the person's canonical identifier.
	ID string `json:"id"`
	// Name is the person's display name.
	Name string `json:"name"`
	// Deactivated marks a person whose every known account is deactivated.
	Deactivated bool `json:"deactivated,omitempty"`
}

// AlsoOnRecord lists, for each critical finding, the subject and the other
// people on record for it. A critical finding is a subject resting mainly on
// one person; the rest of its record is where a second person could already
// be, and the list says only who is there, never who should be. Findings come
// in the order given.
func AlsoOnRecord(ix *index.Index, findings []TopicRisk) []SubjectCover {
	holders := subjectHolders(ix, topicGroups(ix))
	out := make([]SubjectCover, 0, len(findings))
	for _, f := range findings {
		if f.Level != "critical" || len(f.Experts) == 0 {
			continue
		}
		restsOn, restsOnID := f.Lead, f.LeadID
		if restsOnID == "" {
			restsOn, restsOnID = f.Experts[0].Name, f.Experts[0].ID
		}
		cover := SubjectCover{
			Subject: f.Topic, RestsOn: restsOn, RestsOnID: restsOnID,
			AlsoOnRecord: []CoverPerson{},
		}
		for _, id := range slices.Sorted(maps.Keys(holders[f.Topic])) {
			if string(id) == restsOnID {
				continue
			}
			cover.AlsoOnRecord = append(cover.AlsoOnRecord, CoverPerson{
				ID: string(id), Name: personName(ix, id), Deactivated: isDeactivated(ix, id),
			})
		}
		sort.SliceStable(cover.AlsoOnRecord, func(i, j int) bool {
			return strings.ToLower(cover.AlsoOnRecord[i].Name) < strings.ToLower(cover.AlsoOnRecord[j].Name)
		})
		out = append(out, cover)
	}
	return out
}
