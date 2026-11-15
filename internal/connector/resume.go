package connector

import (
	"maps"
	"time"
)

// resumeMargin is how far behind its own start a read places its cursor. A
// listing can lag the change it reports, and the reader's clock can run ahead
// of the site's, so the next read asks from a little earlier and the Seen set
// keeps the overlap from counting twice.
const resumeMargin = 2 * time.Minute

// queryOverlap is how far below its cursor a Jira or Confluence query reaches.
// Both ask from the cursor less a two-minute margin, written to the minute, so
// an item up to three minutes older than the cursor comes back again.
const queryOverlap = 3 * time.Minute

// wholeSource is the unit name of a source that reads as one query, so it
// keeps a single position.
const wholeSource = ""

// Position is where a read of one unit stopped, so a later read resumes there.
// A unit is whatever a source reads independently: a Slack channel, a GitHub
// repository, or the whole of a source that runs one query.
type Position struct {
	// Cursor is the time the next read asks from. It keeps the zone the site
	// wrote it in, which a site that reads dates as wall-clock time depends on.
	Cursor time.Time
	// Mark is an exact position only the source understands, such as the
	// timestamp string of the newest Slack message read.
	Mark string
	// Seen maps each item read at or after the next read's lower bound to the
	// update stamp it carried, so the next read drops the ones that come back
	// unchanged instead of counting them again.
	Seen map[string]string
}

// Resumable is implemented by sources that can read only what changed since an
// earlier read. The positions are collected during Fetch and read afterwards,
// as episodes are, so the Source interface stays one method.
type Resumable interface {
	// Positions returns where the most recent Fetch stopped in each unit it
	// read to the end. A unit it could not read is absent, so that unit keeps
	// whatever position an earlier read saved.
	Positions() map[string]Position
	// Skipped names the units the most recent Fetch could not read.
	Skipped() []string
}

// boundary tracks the dated items one read of a unit observed and builds the
// position the next read resumes from.
type boundary struct {
	// prev is the position this read resumed from, zero on a first read.
	prev Position
	// limit is the latest time the next cursor may take.
	limit time.Time
	// overlap is how far below the cursor the source's next query reaches.
	overlap time.Duration
	// newest is the latest update time observed.
	newest time.Time
	// items maps every observed item to its update time.
	items map[string]time.Time
}

// newBoundary starts tracking a read that began at start and resumed from
// prev. The overlap is how far below the cursor the source's next query
// reaches back, zero when the query bound is exact.
func newBoundary(prev Position, start time.Time, overlap time.Duration) *boundary {
	return &boundary{
		prev: prev, limit: start.Add(-resumeMargin), overlap: overlap,
		items: make(map[string]time.Time),
	}
}

// stamp renders an update time as the string Seen compares. It is the instant,
// so the same moment written in two zones compares equal.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// counted notes an item this read observed and reports whether the read that
// wrote the previous position already counted it unchanged. The caller skips
// an item that was.
func (b *boundary) counted(key string, updated time.Time) bool {
	b.items[key] = updated
	if updated.After(b.newest) {
		b.newest = updated
	}
	return b.prev.Seen[key] == stamp(updated)
}

// position returns where the next read resumes. The cursor is the newest
// update observed, held back to the read's own start so an item changed while
// the read ran is asked for again, and never behind the previous cursor. A read
// that observed nothing keeps the previous position. With no previous position
// either, it returns the zero Position, and the caller decides what an empty
// first read means for its source.
func (b *boundary) position() Position {
	if b.newest.IsZero() {
		return b.prev
	}
	cursor := b.newest
	if cursor.After(b.limit) {
		cursor = b.limit.In(b.newest.Location())
	}
	if b.prev.Cursor.After(cursor) {
		cursor = b.prev.Cursor
	}
	floor := cursor.Add(-b.overlap)
	seen := make(map[string]string)
	for key, updated := range b.items {
		if !updated.Before(floor) {
			seen[key] = stamp(updated)
		}
	}
	return Position{Cursor: cursor, Seen: seen}
}

// clonePositions returns a copy of positions that shares no map with it.
func clonePositions(positions map[string]Position) map[string]Position {
	out := make(map[string]Position, len(positions))
	for unit, p := range positions {
		p.Seen = maps.Clone(p.Seen)
		out[unit] = p
	}
	return out
}

// singlePosition returns the positions of a source that reads as one query. A
// read that saw nothing dated has no position to report, so the next run reads
// in full, which is right because nothing is held from an empty read.
func singlePosition(p Position) map[string]Position {
	if p.Cursor.IsZero() {
		return map[string]Position{}
	}
	p.Seen = maps.Clone(p.Seen)
	return map[string]Position{wholeSource: p}
}
