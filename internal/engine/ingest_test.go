package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/whotoask/internal/connector"
	"github.com/dcadolph/whotoask/internal/index"
	"github.com/dcadolph/whotoask/internal/policy"
	"github.com/dcadolph/whotoask/internal/state"
	"github.com/dcadolph/whotoask/internal/vault"
)

// TestGuardArchive covers the gate on keeping the words of a conversation. It
// is the only thing standing between a run and full conversation text on disk.
func TestGuardArchive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Indexer func(t *testing.T) *Indexer
		Want    error
	}{{ // Test 0: The organization's policy forbids retaining content.
		Indexer: func(t *testing.T) *Indexer {
			return &Indexer{Dir: Dir(t.TempDir()), Policy: policy.New(policy.Open, false).WithoutArchive()}
		},
		Want: ErrBadArgs,
	}, { // Test 1: Allowed by policy, but no key is set to encrypt it.
		Indexer: func(t *testing.T) *Indexer {
			return &Indexer{Dir: Dir(t.TempDir()), Policy: policy.New(policy.Open, false)}
		},
		Want: ErrBadArgs,
	}, { // Test 2: Allowed by policy and encrypted, with no license anywhere.
		Indexer: func(t *testing.T) *Indexer {
			c := testKeyCodec(t)
			return &Indexer{
				Dir: Dir(t.TempDir()), Policy: policy.New(policy.Open, false),
				Codec: func() (vault.Codec, error) { return c, nil },
			}
		},
		Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			x := test.Indexer(t)
			x.Log = &bytes.Buffer{}
			if err := x.guardArchive(); !errors.Is(err, test.Want) {
				t.Errorf("guardArchive = %v, want %v", err, test.Want)
			}
		})
	}
}

// testResumable is a source that reports fixed positions and skipped units, for
// tests of how the engine stores them.
type testResumable struct {
	// positions is what Positions returns.
	positions map[string]connector.Position
	// skipped is what Skipped returns.
	skipped []string
}

// Fetch returns no records.
func (s *testResumable) Fetch(context.Context) ([]connector.Record, error) { return nil, nil }

// Positions returns the fixed positions.
func (s *testResumable) Positions() map[string]connector.Position { return s.positions }

// Skipped returns the fixed skipped units.
func (s *testResumable) Skipped() []string { return s.skipped }

// TestSavePositionsConcurrent verifies that concurrent index runs against
// different sources do not drop each other's positions. The state file is a
// shared read-modify-write, so without the file lock two runs race and one
// source's watermark is lost.
func TestSavePositionsConcurrent(t *testing.T) {
	t.Parallel()
	x := &Indexer{Dir: Dir(t.TempDir())}
	sources := []string{"jira", "confluence", "slack", "github"}
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	for i, src := range sources {
		wg.Add(1)
		go func(i int, src string) {
			defer wg.Done()
			at := base.Add(time.Duration(i) * time.Hour)
			res := &testResumable{positions: map[string]connector.Position{"": {Cursor: at}}}
			if err := x.savePositions(src, "", false, res); err != nil {
				t.Errorf("savePositions(%s): %v", src, err)
			}
		}(i, src)
	}
	wg.Wait()

	st, err := NewStore(x.Dir, nil).LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	for i, src := range sources {
		wm, ok := st.Get(src, "")
		if !ok {
			t.Errorf("source %q positions lost to a concurrent write", src)
			continue
		}
		if want := base.Add(time.Duration(i) * time.Hour); !wm.Units[""].Cursor.Equal(want) {
			t.Errorf("source %q cursor = %v, want %v", src, wm.Units[""].Cursor, want)
		}
	}
}

// TestSavePositionsKeepsSkippedUnits verifies the rule the whole design rests
// on. An incremental run that could not read a unit leaves that unit's position
// where it was, so the next run reads it from there, and a full run that could
// not read a unit leaves it with no position, so the next run reads it whole.
func TestSavePositionsKeepsSkippedUnits(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	x := &Indexer{Dir: Dir(t.TempDir()), Now: func() time.Time { return clock }}
	units := func() map[string]state.Unit {
		t.Helper()
		st, err := NewStore(x.Dir, nil).LoadState()
		if err != nil {
			t.Fatalf("LoadState: %v", err)
		}
		wm, _ := st.Get("slack", "public")
		return wm.Units
	}
	full := &testResumable{positions: map[string]connector.Position{
		"C1": {Mark: "100.000001"}, "C2": {Mark: "200.000001"},
	}}
	if err := x.savePositions("slack", "public", false, full); err != nil {
		t.Fatalf("full read: %v", err)
	}

	// Test 0: An incremental run that skipped C2 moves C1 and leaves C2 alone.
	clock = clock.Add(time.Hour)
	partial := &testResumable{
		positions: map[string]connector.Position{"C1": {Mark: "150.000001"}},
		skipped:   []string{"C2"},
	}
	if err := x.savePositions("slack", "public", true, partial); err != nil {
		t.Fatalf("incremental read: %v", err)
	}
	want := map[string]state.Unit{"C1": {Mark: "150.000001"}, "C2": {Mark: "200.000001"}}
	if diff := cmp.Diff(want, units()); diff != "" {
		t.Errorf("after a skipped unit (-want +got):\n%s", diff)
	}
	st, _ := NewStore(x.Dir, nil).LoadState()
	if wm, _ := st.Get("slack", "public"); wm.Complete || !wm.FullAt.Equal(clock.Add(-time.Hour)) {
		t.Errorf("complete = %v, full at = %v, want an incomplete run that keeps the full read's time",
			wm.Complete, wm.FullAt)
	}

	// Test 1: A full run that skipped C2 leaves it with no position at all.
	if err := x.savePositions("slack", "public", false, partial); err != nil {
		t.Fatalf("second full read: %v", err)
	}
	want = map[string]state.Unit{"C1": {Mark: "150.000001"}}
	if diff := cmp.Diff(want, units()); diff != "" {
		t.Errorf("after a full read that skipped a unit (-want +got):\n%s", diff)
	}
}

// TestResumeForcesAFullReadWhenDue verifies a run resumes from the saved
// positions while the last full read is recent and reads in full once it is
// older than the schedule allows, or when no position was ever saved.
func TestResumeForcesAFullReadWhenDue(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		Saved           bool
		After           time.Duration
		WantIncremental bool
	}{{ // Test 0: A full read from an hour ago resumes.
		Saved: true, After: time.Hour, WantIncremental: true,
	}, { // Test 1: A full read just inside the schedule resumes.
		Saved: true, After: fullEvery, WantIncremental: true,
	}, { // Test 2: A full read older than the schedule reads in full.
		Saved: true, After: fullEvery + time.Minute, WantIncremental: false,
	}, { // Test 3: Nothing saved reads in full.
		Saved: false, After: time.Hour, WantIncremental: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			clock := start
			x := &Indexer{Dir: Dir(t.TempDir()), Now: func() time.Time { return clock }}
			if err := os.WriteFile(x.Dir.Index(), []byte("{}"), 0o600); err != nil {
				t.Fatalf("write index: %v", err)
			}
			if test.Saved {
				res := &testResumable{positions: map[string]connector.Position{"C1": {Mark: "100.000001"}}}
				if err := x.savePositions("slack", "public", false, res); err != nil {
					t.Fatalf("savePositions: %v", err)
				}
			}
			clock = start.Add(test.After)
			req := &IndexRequest{Source: "slack", IndexArgs: IndexArgs{Merge: true}}
			positions, incremental, err := x.resume(req, true, "public")
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			if incremental != test.WantIncremental {
				t.Errorf("incremental = %v, want %v", incremental, test.WantIncremental)
			}
			if incremental && positions["C1"].Mark != "100.000001" {
				t.Errorf("positions = %v, want the saved mark for C1", positions)
			}
		})
	}
}

// TestIndexerUnlock verifies a merge into a sealed index asks Unlock for the
// key, keeps asking until a candidate opens the index, and saves under the key
// that worked. With no Unlock the sealed error stands and nothing is written.
func TestIndexerUnlock(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantLabels  []string
		WantRecords []string
		Want        error
		NoUnlock    bool
	}{{ // Test 0: A wrong key, then the right one.
		WantLabels:  []string{IndexPassphrase},
		WantRecords: []string{"org-csv file:"},
		Want:        nil,
	}, { // Test 1: No Unlock leaves the sealed error and records nothing.
		NoUnlock: true,
		Want:     vault.ErrEncrypted,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			csv := filepath.Join(dir, "people.csv")
			content := "name,email,title,team,topics\n" +
				"Jane Doe,jane@x.com,Engineer,Billing,retries\n" +
				"Raj Patel,raj@x.com,Engineer,Billing,invoices\n"
			if err := os.WriteFile(csv, []byte(content), 0o600); err != nil {
				t.Fatalf("write csv: %v", err)
			}
			key := testKeyCodec(t)
			sealed := &Indexer{Dir: Dir(dir), Codec: func() (vault.Codec, error) { return key, nil }}
			req := IndexRequest{Source: "org-csv", IndexArgs: IndexArgs{File: csv}}
			if err := sealed.Index(context.Background(), req); err != nil {
				t.Fatalf("seal: %v", err)
			}
			before, err := os.ReadFile(sealed.Dir.Index())
			if err != nil {
				t.Fatalf("read index: %v", err)
			}

			var labels, records []string
			x := &Indexer{Dir: Dir(dir), Log: &bytes.Buffer{}}
			x.Record = func(source, scope string) error {
				records = append(records, source+" "+scope[:len("file:")])
				return nil
			}
			if !test.NoUnlock {
				x.Unlock = func(label string, try func(vault.Codec) error) (vault.Codec, error) {
					labels = append(labels, label)
					if err := try(vault.NewPassphraseCipher([]byte("wrong"))); err == nil {
						t.Error("a wrong passphrase opened the index")
					}
					if err := try(key); err != nil {
						return nil, err
					}
					return key, nil
				}
			}
			req.Merge = true
			err = x.Index(context.Background(), req)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Index = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantLabels, labels); diff != "" {
				t.Errorf("unlock labels mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantRecords, records); diff != "" {
				t.Errorf("records mismatch (-want +got):\n%s", diff)
			}
			after, err := os.ReadFile(x.Dir.Index())
			if err != nil {
				t.Fatalf("read index after: %v", err)
			}
			if test.Want != nil && !bytes.Equal(before, after) {
				t.Error("a failed unlock rewrote the index")
			}
			if _, err := NewStore(x.Dir, nil).LoadIndex(); !errors.Is(err, vault.ErrEncrypted) {
				t.Errorf("plain load after the run = %v, want the index still sealed", err)
			}
			if _, err := NewStore(x.Dir, key).LoadIndex(); err != nil {
				t.Errorf("keyed load after the run: %v", err)
			}
		})
	}
}

// testKeyCodec returns an index cipher with a fixed test key.
func testKeyCodec(t *testing.T) vault.Codec {
	t.Helper()
	c, err := vault.NewKeyCipher(make([]byte, 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

// changeRosters are org charts indexed in turn: a first build, a person and a
// team joining, the team going, and a rerun that changes nothing.
var changeRosters = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
	"Jane Doe,jane@x.com,Engineer,Billing,retries\nRaj Patel,raj@x.com,Engineer,Billing,invoices\n",
	"Jane Doe,jane@x.com,Engineer,Billing,retries\nRaj Patel,raj@x.com,Engineer,Billing,invoices\n" +
		"Ann Lee,ann@x.com,Engineer,Payments,ledger\n",
	"Jane Doe,jane@x.com,Engineer,Billing,retries\nRaj Patel,raj@x.com,Engineer,Billing,invoices\n" +
		"Ann Lee,ann@x.com,Engineer,Billing,ledger\n",
	"Jane Doe,jane@x.com,Engineer,Billing,retries\nRaj Patel,raj@x.com,Engineer,Billing,invoices\n" +
		"Ann Lee,ann@x.com,Engineer,Billing,ledger\n",
}

// indexRoster indexes one roster from the org CSV source into dir under codec
// with the indexer clock pinned to at.
func indexRoster(t *testing.T, dir string, codec vault.Codec, roster string, at time.Time) {
	t.Helper()
	csv := filepath.Join(t.TempDir(), "people.csv")
	if err := os.WriteFile(csv, []byte("name,email,title,team,topics\n"+roster), 0o600); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	x := &Indexer{
		Dir: Dir(dir), Codec: func() (vault.Codec, error) { return codec, nil },
		Now: func() time.Time { return at },
	}
	if err := x.Index(context.Background(), IndexRequest{Source: "org-csv", IndexArgs: IndexArgs{File: csv}}); err != nil {
		t.Fatalf("index: %v", err)
	}
}

// TestFoldLogsChanges verifies a first build logs nothing, each later fold
// that changes the graph appends one entry stamped with the indexer clock, a
// fold that changes nothing appends none, and a sealed store seals the log.
func TestFoldLogsChanges(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		Codec       vault.Codec
		WantEntries []ChangeEntry
	}{{ // Test 0: Plain store.
		WantEntries: []ChangeEntry{{
			At:      start.Add(time.Hour),
			Changes: index.Changes{PeopleJoined: []string{"Ann Lee"}, TeamsAdded: []string{"Payments"}},
		}, {
			At:      start.Add(2 * time.Hour),
			Changes: index.Changes{TeamsRemoved: []string{"Payments"}},
		}},
	}, { // Test 1: Sealed store.
		Codec: testKeyCodec(t),
		WantEntries: []ChangeEntry{{
			At:      start.Add(time.Hour),
			Changes: index.Changes{PeopleJoined: []string{"Ann Lee"}, TeamsAdded: []string{"Payments"}},
		}, {
			At:      start.Add(2 * time.Hour),
			Changes: index.Changes{TeamsRemoved: []string{"Payments"}},
		}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			indexRoster(t, dir, test.Codec, changeRosters[0], start)
			if _, err := os.Stat(Dir(dir).Changes()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a first build wrote a change log: %v", err)
			}
			for i, roster := range changeRosters[1:] {
				indexRoster(t, dir, test.Codec, roster, start.Add(time.Duration(i+1)*time.Hour))
			}
			raw, err := os.ReadFile(Dir(dir).Changes())
			if err != nil {
				t.Fatalf("read change log: %v", err)
			}
			var plain []ChangeEntry
			parseErr := json.Unmarshal(raw, &plain)
			if sealed := test.Codec != nil; sealed == (parseErr == nil) {
				t.Errorf("sealed %v but the file parses as JSON: %v", sealed, parseErr == nil)
			}
			got, err := NewStore(Dir(dir), test.Codec).LoadChanges()
			if err != nil {
				t.Fatalf("LoadChanges: %v", err)
			}
			if diff := cmp.Diff(test.WantEntries, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("change log mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// cancelAtEOF reads like its reader and cancels when the reader runs out, which is a
// connector seeing a cancel after its last read and returning what it has with no error.
type cancelAtEOF struct {
	// r is what gets read.
	r *strings.Reader
	// cancel runs at the end of r.
	cancel context.CancelFunc
}

// Read reads from r and cancels once r has nothing left.
func (c cancelAtEOF) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err != nil {
		c.cancel()
	}
	return n, err
}

// TestIndexCanceledSavesNothing verifies records a connector returned with no error
// after a cancel are not folded, since that would store a partial read as complete.
func TestIndexCanceledSavesNothing(t *testing.T) {
	t.Parallel()
	dir := Dir(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x := &Indexer{
		Dir: dir, Log: io.Discard, Policy: policy.Default(),
		Stdin: cancelAtEOF{r: strings.NewReader(`[{"kind":"person","name":"Ann Lee","email":"ann@x.com"}]`), cancel: cancel},
		Record: func(string, string) error {
			t.Error("a canceled index recorded a run")
			return nil
		},
	}
	if err := x.Index(ctx, IndexRequest{Source: "json"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Index error = %v, want %v", err, context.Canceled)
	}
	for _, file := range dir.Files() {
		if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a canceled index left %s: %v", filepath.Base(file), err)
		}
	}
}

// TestCountRecords verifies a run counts the people the index holds after
// identities are joined, so one person under many keys counts once, and counts
// distinct channels by name.
func TestCountRecords(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Recs         []connector.Record
		WantPeople   int
		WantChannels int
	}{{ // Test 0: Nothing read.
		Recs: nil,
	}, { // Test 1: One person in two records counts once.
		Recs: []connector.Record{
			{Source: "git", PersonID: "git:ann", Email: "ann@x.com", Topics: []string{"ledger"}},
			{Source: "git", PersonID: "git:ann", Email: "ann@x.com", Topics: []string{"ledger"}},
		},
		WantPeople: 1,
	}, { // Test 2: With no source id the email keys the person, in any case.
		Recs: []connector.Record{
			{Source: "git", Email: "Ann@x.com", Topics: []string{"ledger"}},
			{Source: "git", Email: "ann@x.com ", Topics: []string{"ledger"}},
			{Source: "git", Email: "bo@x.com", Topics: []string{"ledger"}},
		},
		WantPeople: 2,
	}, { // Test 3: A person with neither id nor email is not counted.
		Recs:       []connector.Record{{Source: "git", Name: "Nobody", Topics: []string{"ledger"}}},
		WantPeople: 0,
	}, { // Test 4: Channels count by name, and topics count as neither.
		Recs: []connector.Record{
			{Source: "slack", Kind: connector.KindChannel, Name: "general"},
			{Source: "slack", Kind: connector.KindChannel, Name: "general"},
			{Source: "slack", Kind: connector.KindChannel, Name: "billing"},
			{Source: "slack", Kind: connector.KindTopic, Name: "ledger"},
			{Source: "slack", PersonID: "slack:U1", Name: "Ann", Topics: []string{"ledger"}},
		},
		WantPeople: 1, WantChannels: 2,
	}, { // Test 5: A Slack account and its email-keyed author record are one person.
		Recs: []connector.Record{
			{Source: "slack", PersonID: "slack:U1", Email: "ann@x.com", Name: "Ann"},
			{Source: "slack", PersonID: "ann@x.com", Topics: []string{"ledger"}},
		},
		WantPeople: 1,
	}, { // Test 6: Two addresses joined by an alternate identity are one person.
		Recs: []connector.Record{
			{Source: "git", PersonID: "git:ann", Email: "ann@x.com", Topics: []string{"ledger"}},
			{Source: "git", PersonID: "git:ann2", Email: "ann@home.org", AltIDs: []string{"ann@x.com"},
				Topics: []string{"ledger"}},
		},
		WantPeople: 1,
	}, { // Test 7: A CODEOWNERS team is not a person.
		Recs: []connector.Record{
			{Source: "codeowners", PersonID: "codeowners:acme/ledger-squad", Name: "@acme/ledger-squad",
				Topics: []string{"ledger"}},
			{Source: "codeowners", PersonID: "codeowners:zoe", Name: "@zoe", Topics: []string{"ledger"}},
		},
		WantPeople: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ix := index.New()
			ix.Add(test.Recs)
			ix.Canonicalize()
			people, channels := countRecords(ix, test.Recs)
			if people != test.WantPeople || channels != test.WantChannels {
				t.Errorf("counted %d people and %d channels, want %d and %d",
					people, channels, test.WantPeople, test.WantChannels)
			}
			if people > len(ix.Graph.People) {
				t.Errorf("counted %d people, more than the %d the index holds", people, len(ix.Graph.People))
			}
		})
	}
}

// TestHeldRecords verifies an incremental run that fetched nothing still counts
// what the index holds for its source, and leaves other sources out.
func TestHeldRecords(t *testing.T) {
	t.Parallel()
	ix := index.New()
	ix.Add([]connector.Record{
		{Source: "git", PersonID: "git:ann", Email: "ann@x.com", Topics: []string{"ledger"}},
		{Source: "git", PersonID: "git:bo", Email: "bo@x.com", Topics: []string{"billing"}},
		{Source: "org-csv", PersonID: "csv:cy", Email: "cy@x.com", Topics: []string{"payments"}},
	})
	ix.MergeIncremental(nil)
	if people, _ := countRecords(ix, heldRecords(ix, "git", nil)); people != 2 {
		t.Errorf("git holds %d people after an empty increment, want 2", people)
	}
	fetched := []connector.Record{{Source: "org-csv", PersonID: "csv:cy", Email: "cy@x.com"}}
	if people, _ := countRecords(ix, heldRecords(ix, "org-csv", fetched)); people != 1 {
		t.Errorf("org-csv holds %d people, want 1", people)
	}
}
