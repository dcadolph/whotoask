package connector

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestBoundaryPosition verifies where a read says the next one resumes: at the
// newest update it observed, held back to its own start, never behind the
// position it resumed from, with the items the next query will return again
// remembered so they are not counted twice.
func TestBoundaryPosition(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return start.Add(d) }
	tests := []struct {
		Prev       Position
		Overlap    time.Duration
		Items      map[string]time.Time
		WantResult Position
	}{{ // Test 0: The cursor is the newest update, and only the item at it is remembered.
		Items: map[string]time.Time{"a": at(-3 * time.Hour), "b": at(-time.Hour)},
		WantResult: Position{
			Cursor: at(-time.Hour), Seen: map[string]string{"b": stamp(at(-time.Hour))},
		},
	}, { // Test 1: An item updated after the read began holds the cursor at the start less the margin.
		Items: map[string]time.Time{"a": at(-time.Hour), "b": at(time.Minute)},
		WantResult: Position{
			Cursor: at(-resumeMargin), Seen: map[string]string{"b": stamp(at(time.Minute))},
		},
	}, { // Test 2: An overlap remembers the items the next query reaches back for.
		Overlap: queryOverlap,
		Items: map[string]time.Time{
			"a": at(-time.Hour), "b": at(-time.Hour - 2*time.Minute), "c": at(-2 * time.Hour),
		},
		WantResult: Position{
			Cursor: at(-time.Hour),
			Seen: map[string]string{
				"a": stamp(at(-time.Hour)), "b": stamp(at(-time.Hour - 2*time.Minute)),
			},
		},
	}, { // Test 3: A read that observed nothing keeps the position it resumed from.
		Prev:       Position{Cursor: at(-time.Hour), Seen: map[string]string{"a": stamp(at(-time.Hour))}},
		WantResult: Position{Cursor: at(-time.Hour), Seen: map[string]string{"a": stamp(at(-time.Hour))}},
	}, { // Test 4: A first read that observed nothing has no position.
		WantResult: Position{},
	}, { // Test 5: The cursor never moves behind the one the read resumed from.
		Prev:  Position{Cursor: at(-time.Hour)},
		Items: map[string]time.Time{"late": at(-90 * time.Minute)},
		WantResult: Position{
			Cursor: at(-time.Hour), Seen: map[string]string{},
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			b := newBoundary(test.Prev, start, test.Overlap)
			for key, updated := range test.Items {
				b.counted(key, updated)
			}
			if diff := cmp.Diff(test.WantResult, b.position(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("position (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBoundaryCounted verifies an item is left out only when it comes back with
// the same update stamp it was counted with. A changed item is a real change.
func TestBoundaryCounted(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	elsewhere := when.In(time.FixedZone("IST", 5*3600+1800))
	prev := Position{Cursor: when, Seen: map[string]string{"a": stamp(when)}}
	tests := []struct {
		Key        string
		Updated    time.Time
		WantResult bool
	}{{ // Test 0: The same item at the same moment was already counted.
		Key: "a", Updated: when, WantResult: true,
	}, { // Test 1: The same moment written in another zone is still the same moment.
		Key: "a", Updated: elsewhere, WantResult: true,
	}, { // Test 2: The same item changed since, so it counts.
		Key: "a", Updated: when.Add(time.Second), WantResult: false,
	}, { // Test 3: Another item at the same moment counts.
		Key: "b", Updated: when, WantResult: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			b := newBoundary(prev, when.Add(time.Hour), 0)
			if got := b.counted(test.Key, test.Updated); got != test.WantResult {
				t.Errorf("counted(%q, %v) = %v, want %v", test.Key, test.Updated, got, test.WantResult)
			}
		})
	}
}

// TestBoundaryKeepsTheSiteZone verifies a cursor held back to the read's start
// is written in the zone of the site's own timestamps. Jira and Confluence read
// a query date as wall-clock time, so a cursor in another zone asks for the
// wrong hour.
func TestBoundaryKeepsTheSiteZone(t *testing.T) {
	t.Parallel()
	site := time.FixedZone("IST", 5*3600+1800)
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	b := newBoundary(Position{}, start, queryOverlap)
	b.counted("K-1", start.Add(time.Minute).In(site))
	got := b.position().Cursor
	if !got.Equal(start.Add(-resumeMargin)) {
		t.Errorf("cursor = %v, want the read's start less the margin", got)
	}
	if _, offset := got.Zone(); offset != 5*3600+1800 {
		t.Errorf("cursor zone offset = %d, want the site's own %d", offset, 5*3600+1800)
	}
}
