package livestore

import (
	"sync"

	"github.com/google/uuid"
	"github.com/grafana/tempo/tempodb/encoding/common"
)

// walBlockEntry wraps a WAL block with a per-block RWMutex used to coordinate
// concurrent queries with the eventual deletion or transition of the block.
//
// Queries acquire RLock for the duration of their work; deletion acquires the
// Lock and marks the block cleared so any reader that captured a reference
// before the entry was removed from walBlocks can detect it and skip.
type walBlockEntry struct {
	block   common.WALBlock
	mtx     sync.RWMutex
	cleared bool
}

func newWALBlockEntry(b common.WALBlock) *walBlockEntry {
	return &walBlockEntry{block: b}
}

// acquire takes the read lock and returns the underlying block plus a release
// function. If the block has already been cleared, ok is false and the caller
// should skip it (no need to call release).
func (e *walBlockEntry) acquire() (block common.WALBlock, release func(), ok bool) {
	e.mtx.RLock()
	if e.cleared {
		e.mtx.RUnlock()
		return nil, nil, false
	}
	return e.block, e.mtx.RUnlock, true
}

// clear waits for any in-flight readers, marks the entry cleared, and invokes
// fn (typically the underlying block's Clear method) while holding the write
// lock. Subsequent calls are no-ops. fn may be nil.
func (e *walBlockEntry) clear(fn func(common.WALBlock) error) error {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	if e.cleared {
		return nil
	}
	e.cleared = true
	if fn == nil {
		return nil
	}
	return fn(e.block)
}

// completeBlockEntry mirrors walBlockEntry for completed blocks.
type completeBlockEntry struct {
	block   *LocalBlock
	mtx     sync.RWMutex
	cleared bool
}

func newCompleteBlockEntry(b *LocalBlock) *completeBlockEntry {
	return &completeBlockEntry{block: b}
}

func (e *completeBlockEntry) acquire() (block *LocalBlock, release func(), ok bool) {
	e.mtx.RLock()
	if e.cleared {
		e.mtx.RUnlock()
		return nil, nil, false
	}
	return e.block, e.mtx.RUnlock, true
}

func (e *completeBlockEntry) clear(fn func(*LocalBlock) error) error {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	if e.cleared {
		return nil
	}
	e.cleared = true
	if fn == nil {
		return nil
	}
	return fn(e.block)
}

// walBlockMap is a typed wrapper around sync.Map keyed by uuid.UUID storing
// *walBlockEntry. The wrapper centralises the type assertions and provides
// the small surface used by the livestore.
type walBlockMap struct {
	m sync.Map
}

func (s *walBlockMap) load(id uuid.UUID) (*walBlockEntry, bool) {
	v, ok := s.m.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*walBlockEntry), true
}

func (s *walBlockMap) store(id uuid.UUID, e *walBlockEntry) {
	s.m.Store(id, e)
}

func (s *walBlockMap) delete(id uuid.UUID) {
	s.m.Delete(id)
}

// snapshot returns a copy of all entries currently in the map. The result is
// not a strictly consistent snapshot — entries inserted or removed during
// iteration may or may not be present — but it is a reasonable best effort
// from the underlying sync.Map.
func (s *walBlockMap) snapshot() map[uuid.UUID]*walBlockEntry {
	out := map[uuid.UUID]*walBlockEntry{}
	s.m.Range(func(k, v any) bool {
		out[k.(uuid.UUID)] = v.(*walBlockEntry)
		return true
	})
	return out
}

func (s *walBlockMap) count() int {
	n := 0
	s.m.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// loadBlock returns the underlying WAL block for id, or nil if no entry
// exists. It is intended for callers (mostly tests) that just want the block
// reference and don't need to coordinate with deletion via the per-entry
// lock.
func (s *walBlockMap) loadBlock(id uuid.UUID) common.WALBlock {
	if e, ok := s.load(id); ok {
		return e.block
	}
	return nil
}

// completeBlockMap mirrors walBlockMap for *completeBlockEntry.
type completeBlockMap struct {
	m sync.Map
}

func (s *completeBlockMap) load(id uuid.UUID) (*completeBlockEntry, bool) {
	v, ok := s.m.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*completeBlockEntry), true
}

func (s *completeBlockMap) store(id uuid.UUID, e *completeBlockEntry) {
	s.m.Store(id, e)
}

func (s *completeBlockMap) delete(id uuid.UUID) {
	s.m.Delete(id)
}

func (s *completeBlockMap) snapshot() map[uuid.UUID]*completeBlockEntry {
	out := map[uuid.UUID]*completeBlockEntry{}
	s.m.Range(func(k, v any) bool {
		out[k.(uuid.UUID)] = v.(*completeBlockEntry)
		return true
	})
	return out
}

func (s *completeBlockMap) count() int {
	n := 0
	s.m.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// loadBlock returns the underlying *LocalBlock for id, or nil if no entry
// exists.
func (s *completeBlockMap) loadBlock(id uuid.UUID) *LocalBlock {
	if e, ok := s.load(id); ok {
		return e.block
	}
	return nil
}
