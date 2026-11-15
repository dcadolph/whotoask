package engine

import (
	"errors"
	"maps"
	"time"

	"github.com/dcadolph/whotoask/internal/connector"
	"github.com/dcadolph/whotoask/internal/state"
	"github.com/dcadolph/whotoask/internal/util"
	"github.com/dcadolph/whotoask/internal/vault"
)

// loadState reads the watermarks, calling Unlock when they are sealed under a key
// the store does not have, since a run reads them before it loads the index.
func (x *Indexer) loadState() (*state.State, error) {
	st, err := x.store()
	if err != nil {
		return nil, err
	}
	wm, err := st.LoadState()
	if !errors.Is(err, vault.ErrEncrypted) || x.Unlock == nil {
		return wm, err
	}
	c, err := x.Unlock(IndexPassphrase, func(c vault.Codec) error {
		var lerr error
		wm, lerr = st.WithCodec(c).LoadState()
		return lerr
	})
	if err != nil {
		return nil, err
	}
	x.unlocked = c
	return wm, nil
}

// incrementalCapable reports whether a source supports incremental
// re-indexing, fetching only what changed since a watermark. Other sources
// read in full.
func incrementalCapable(source string) bool {
	spec, ok := SourceByName(source)
	return ok && spec.Incremental
}

// fullEvery is how long a source goes between full reads. An incremental read
// counts an item again each time it is edited, and only a full read replaces
// what the source holds with one clean count, so one is forced on this
// schedule to keep that drift bounded.
const fullEvery = 7 * 24 * time.Hour

// resume decides whether this run is incremental: a merge into an existing
// index of a source that supports it, with saved positions, a full read recent
// enough, and no --full. When so it returns where the last read stopped in
// each unit, so the run fetches only what changed since and folds it in rather
// than re-reading the whole source.
func (x *Indexer) resume(
	req *IndexRequest, canInc bool, scope string,
) (map[string]connector.Position, bool, error) {
	if !req.Merge || req.Full || !canInc || !x.indexExists() {
		return nil, false, nil
	}
	st, err := x.loadState()
	if err != nil {
		return nil, false, err
	}
	wm, ok := st.Get(req.Source, scope)
	if !ok || wm.Units == nil || x.now().Sub(wm.FullAt) > fullEvery {
		return nil, false, nil
	}
	positions := make(map[string]connector.Position, len(wm.Units))
	for unit, u := range wm.Units {
		positions[unit] = connector.Position{Cursor: u.Cursor, Mark: u.Mark, Seen: u.Seen}
	}
	return positions, true, nil
}

// savePositions records where a source's read stopped in each unit, after a
// successful index. The source reports its own positions, since only it knows
// which units it read to the end. An incremental run adds the units it finished
// to the saved set, so a unit it skipped keeps its old position and the next
// run resumes it from where it really stopped. A full run replaces the set, so
// a unit it skipped has no position and the next run reads it from the start.
// It does nothing for a source that does not report positions.
func (x *Indexer) savePositions(source, scope string, incremental bool, src connector.Source) error {
	res, ok := src.(connector.Resumable)
	if !ok || !incrementalCapable(source) {
		return nil
	}
	// Serialize the read-modify-write on the state file, so two concurrent
	// index runs against different sources cannot drop each other's positions.
	unlock, err := util.LockFile(x.Dir.State() + util.LockSuffix)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := x.loadState()
	if err != nil {
		return err
	}
	store, err := x.store()
	if err != nil {
		return err
	}
	now := x.now()
	wm := state.Watermark{
		Source: source, Scope: scope, RanAt: now, FullAt: now,
		Complete: len(res.Skipped()) == 0, Units: make(map[string]state.Unit),
	}
	if prev, ok := st.Get(source, scope); ok && incremental {
		wm.FullAt = prev.FullAt
		maps.Copy(wm.Units, prev.Units)
	}
	for unit, p := range res.Positions() {
		wm.Units[unit] = state.Unit{Cursor: p.Cursor, Mark: p.Mark, Seen: p.Seen}
	}
	for _, u := range wm.Units {
		if u.Cursor.After(wm.Cursor) {
			wm.Cursor = u.Cursor
		}
	}
	st.Set(wm)
	return store.SaveState(st)
}

// gitMarks returns where a previous run stopped in each repository, so reading
// resumes there. It is empty unless this is a merge, since a full read is meant
// to start over, and empty on the first run, when there is nothing to resume
// from.
func (x *Indexer) gitMarks(merge bool, paths []string) map[string]string {
	if !merge || !x.indexExists() {
		return nil
	}
	st, err := x.loadState()
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		if wm, ok := st.Get("git", "repo:"+p); ok && wm.Mark != "" {
			out[p] = wm.Mark
		}
	}
	return out
}

// saveGitMarks records where reading stopped in each repository. Each is its
// own watermark, since one run may cover several repositories and they advance
// independently.
func (x *Indexer) saveGitMarks(marks map[string]string) error {
	if len(marks) == 0 {
		return nil
	}
	unlock, err := util.LockFile(x.Dir.State() + util.LockSuffix)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := x.loadState()
	if err != nil {
		return err
	}
	store, err := x.store()
	if err != nil {
		return err
	}
	for path, mark := range marks {
		st.Set(state.Watermark{
			Source: "git", Scope: "repo:" + path, Mark: mark,
			Complete: true, RanAt: x.now(),
		})
	}
	return store.SaveState(st)
}
