package traceql

import (
	"sync/atomic"
)

const SpanPruningAttribute = "aggregation.is_summary"

// NewSpanPruningWatcher returns a watcher that reports whether any matched span is a span-pruning summary span.
func NewSpanPruningWatcher() SpanWatcher {
	return NewAttributePresenceWatcher(NewAttribute(SpanPruningAttribute), SpanPruningAttribute)
}

// SpanWatcher inspects spans as they flow through the TraceQL engine and records something about them.
type SpanWatcher interface {
	// Conditions returns the fetch conditions the watcher needs so the attributes it cares about are loaded onto watched spans.
	Conditions() []Condition
	// WatchSpan inspects a single span.
	// It returns true while the watcher is still interested in further spans.
	// Once it has returned false it must keep returning false without doing further
	// work: spanWatchers keeps calling every watcher rather than tracking which ones
	// are done, which is what lets it stay lock-free.
	WatchSpan(Span) bool
	// Active reports whether the watcher still wants to see spans.
	Active() bool
	// Stats returns the metrics gathered so far, keyed by metric name.
	Stats() map[string]int64
}

var _ SpanWatcher = (*attrPresenceWatcher)(nil)

type attrPresenceWatcher struct {
	attr      Attribute
	metricKey string
	active    atomic.Bool
}

// NewAttributePresenceWatcher returns an watcher that records whether any watched span carries attr.
// When the attribute is seen, Stats reports a count of 1 under metricKey.
func NewAttributePresenceWatcher(attr Attribute, metricKey string) SpanWatcher {
	o := &attrPresenceWatcher{attr: attr, metricKey: metricKey}
	o.active.Store(true)
	return o
}

func (a *attrPresenceWatcher) Conditions() []Condition {
	return []Condition{{Attribute: a.attr, Op: OpNone, CallBack: a.active.Load}}
}

func (a *attrPresenceWatcher) WatchSpan(span Span) bool {
	if !a.active.Load() {
		return false // already found; no longer interested
	}
	if _, ok := span.AttributeFor(a.attr); ok {
		a.active.Store(false)
		return false // found it; done
	}
	return true // keep looking
}

func (a *attrPresenceWatcher) Active() bool {
	return a.active.Load()
}

func (a *attrPresenceWatcher) Stats() map[string]int64 {
	if a.active.Load() {
		return nil
	}
	return map[string]int64{a.metricKey: 1}
}

// spanWatchers holds the watchers for one request. obs is written only by Add during
// compilation and is read-only once evaluation starts, so no lock is needed here: the
// container itself is immutable and each watcher owns whatever state it mutates.
// Watchers that have lost interest stay in obs and cheaply self-reject in WatchSpan,
// which keeps their Stats readable and avoids the shared bookkeeping a partition needs.
type spanWatchers struct {
	obs []SpanWatcher
}

// Add registers watchers. It must be called during compilation, before evaluation
// starts; obs is read without synchronization from then on.
func (s *spanWatchers) Add(watchers ...SpanWatcher) {
	s.obs = append(s.obs, watchers...)
}

func (s *spanWatchers) Conditions() []Condition {
	// Only active watchers need their attributes fetched.
	conds := make([]Condition, 0, len(s.obs))
	for _, watcher := range s.obs {
		if watcher.Active() {
			conds = append(conds, watcher.Conditions()...)
		}
	}
	return conds
}

func (s *spanWatchers) WatchSpans(spans []*Spanset) {
	for _, ss := range spans {
		for _, span := range ss.Spans {
			if !s.WatchSpan(span) {
				return // nobody is interested anymore
			}
		}
	}
}

// WatchSpan feeds a single span to every watcher and reports whether any remains
// interested, so callers can stop calling once they are all done. Finished watchers
// reject the span themselves (see SpanWatcher.WatchSpan), which costs one call and
// writes no shared bookkeeping, so no lock is required here.
//
// Callers must serialize WatchSpan against itself and against Stats for a given
// spanWatchers; watcher implementations are not required to be internally
// concurrency-safe. The metrics evaluator does this with e.mtx; the search path gets
// a private spanWatchers per ExecuteSearch.
func (s *spanWatchers) WatchSpan(span Span) bool {
	stillActive := false
	for _, o := range s.obs {
		if o.WatchSpan(span) {
			stillActive = true
		}
	}
	return stillActive
}

func (s *spanWatchers) Active() bool {
	for _, o := range s.obs {
		if o.Active() {
			return true
		}
	}
	return false
}

func (s *spanWatchers) Stats() map[string]int64 {
	stats := make(map[string]int64)
	for _, watcher := range s.obs {
		for k, v := range watcher.Stats() {
			stats[k] += v
		}
	}
	return stats
}
