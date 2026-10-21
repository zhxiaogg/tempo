package vcg1_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/backend/local"
	"github.com/grafana/tempo/v3/tempodb/encoding"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
)

// countingReader counts the backend requests and bytes of a block.
type countingReader struct {
	backend.Reader
	reqs, bytes atomic.Int64
}

func (c *countingReader) Read(ctx context.Context, name string, id uuid.UUID, tenant string, ci *backend.CacheInfo) ([]byte, error) {
	b, err := c.Reader.Read(ctx, name, id, tenant, ci)
	c.reqs.Add(1)
	c.bytes.Add(int64(len(b)))
	return b, err
}

func (c *countingReader) ReadRange(ctx context.Context, name string, id uuid.UUID, tenant string, off uint64, buf []byte, ci *backend.CacheInfo) error {
	c.reqs.Add(1)
	c.bytes.Add(int64(len(buf)))
	return c.Reader.ReadRange(ctx, name, id, tenant, off, buf, ci)
}

func (c *countingReader) reset() { c.reqs.Store(0); c.bytes.Store(0) }

type ioStat struct{ reqs, bytes int64 }

func (s ioStat) String() string {
	return fmt.Sprintf("%d req / %.2f MB", s.reqs, float64(s.bytes)/1e6)
}

func (c *countingReader) stats() ioStat { return ioStat{c.reqs.Load(), c.bytes.Load()} }

func openTagBlock(t testing.TB, dir string) (common.BackendBlock, *countingReader) {
	ctx := context.Background()
	tenant := filepath.Base(filepath.Dir(dir))
	raw, _, _, err := local.New(&local.Config{Path: filepath.Dir(filepath.Dir(dir))})
	require.NoError(t, err)
	cr := &countingReader{Reader: backend.NewReader(raw)}
	meta, err := cr.BlockMeta(ctx, uuid.MustParse(filepath.Base(dir)), tenant)
	require.NoError(t, err)
	blk, err := encoding.OpenBlock(meta, cr)
	require.NoError(t, err)
	return blk, cr
}

// vset is a set of tag values (or names) as 64-bit hashes, with readable examples for small sets.
type vset struct {
	h  []uint64
	ex map[uint64]string
	st map[uint64]traceql.Static
}

func newVset() *vset { return &vset{ex: map[uint64]string{}, st: map[uint64]traceql.Static{}} }

func (s *vset) addKey(k string) {
	h := xxhash.Sum64String(k)
	s.h = append(s.h, h)
	if len(s.ex) < 200_000 {
		if _, ok := s.ex[h]; !ok {
			s.ex[h] = k
		}
	}
}

func staticKey(v traceql.Static) string {
	return v.Type.String() + "|" + v.EncodeToString(false)
}

func (s *vset) add(v traceql.Static) {
	k := staticKey(v)
	s.addKey(k)
	if h := xxhash.Sum64String(k); len(s.st) < 200_000 {
		if _, ok := s.st[h]; !ok {
			if v.Type == traceql.TypeString {
				v = traceql.NewStaticString(strings.Clone(v.EncodeToString(false)))
			}
			s.st[h] = v
		}
	}
}

func (s *vset) finish() *vset {
	sort.Slice(s.h, func(i, j int) bool { return s.h[i] < s.h[j] })
	out := s.h[:0]
	for i, x := range s.h {
		if i == 0 || x != s.h[i-1] {
			out = append(out, x)
		}
	}
	s.h = out
	return s
}

// diff returns the hashes only in a and only in b (both finished).
func diff(a, b *vset) (onlyA, onlyB []uint64) {
	i, j := 0, 0
	for i < len(a.h) || j < len(b.h) {
		switch {
		case j >= len(b.h) || (i < len(a.h) && a.h[i] < b.h[j]):
			onlyA = append(onlyA, a.h[i])
			i++
		case i >= len(a.h) || b.h[j] < a.h[i]:
			onlyB = append(onlyB, b.h[j])
			j++
		default:
			i++
			j++
		}
	}
	return
}

func (s *vset) examples(hs []uint64, n int) []string {
	var out []string
	for _, h := range hs {
		if len(out) >= n {
			break
		}
		if k, ok := s.ex[h]; ok {
			if len(k) > 120 {
				k = k[:120] + "..."
			}
			out = append(out, strconv.Quote(k))
		} else {
			out = append(out, fmt.Sprintf("#%x", h))
		}
	}
	return out
}

// tagCompare collects the outcome of every compared call.
type tagCompare struct {
	t       *testing.T
	calls   int
	byKind  map[string]int
	bad     []string
	io      []string
	explain map[string]string // mismatch -> explanation (ground truth)
}

func (c *tagCompare) compare(kind, name string, cgSet, vpSet *vset, cgIO, vpIO ioStat, cgDur, vpDur time.Duration) (onlyCG, onlyVP []uint64) {
	c.calls++
	c.byKind[kind]++
	cgSet.finish()
	vpSet.finish()
	onlyCG, onlyVP = diff(cgSet, vpSet)
	if len(onlyCG)+len(onlyVP) > 0 {
		msg := fmt.Sprintf("%s %s: CG %d vP5 %d, only CG %d %v, only vP5 %d %v", kind, name, len(cgSet.h), len(vpSet.h),
			len(onlyCG), cgSet.examples(onlyCG, 5), len(onlyVP), vpSet.examples(onlyVP, 5))
		c.bad = append(c.bad, msg)
		c.t.Log("MISMATCH " + msg)
	}
	c.io = append(c.io, fmt.Sprintf("%-14s %-60s n=%-8d CG %-24s %-8s vP5 %-24s %s", kind, name, len(cgSet.h), cgIO, cgDur.Round(time.Millisecond), vpIO, vpDur.Round(time.Millisecond)))
	return onlyCG, onlyVP
}

type tagBlocks struct {
	cg, vp   common.BackendBlock
	cgR, vpR *countingReader
	opts     common.SearchOptions
}

// run calls f on both blocks, recording I/O and time.
func (b *tagBlocks) run(f func(blk common.BackendBlock, s *vset) error) (cgSet, vpSet *vset, cgIO, vpIO ioStat, cgDur, vpDur time.Duration, err error) {
	cgSet, vpSet = newVset(), newVset()
	b.cgR.reset()
	st := time.Now()
	if err = f(b.cg, cgSet); err != nil {
		return
	}
	cgDur, cgIO = time.Since(st), b.cgR.stats()
	b.vpR.reset()
	st = time.Now()
	if err = f(b.vp, vpSet); err != nil {
		return
	}
	vpDur, vpIO = time.Since(st), b.vpR.stats()
	return
}

// exists runs a TraceQL search on blk and reports whether any span matches (ground truth).
func exists(ctx context.Context, blk common.BackendBlock, opts common.SearchOptions, q string) (bool, error) {
	f := traceql.NewSpansetFetcherWrapper(func(ctx context.Context, req traceql.FetchSpansRequest) (traceql.FetchSpansResponse, error) {
		return blk.Fetch(ctx, req, opts)
	})
	resp, err := traceql.NewEngine().ExecuteSearch(ctx, &tempopb.SearchRequest{Query: q, Limit: 1, SpansPerSpanSet: 1}, f)
	if err != nil {
		return false, err
	}
	return len(resp.Traces) > 0, nil
}

func literal(v traceql.Static) string {
	if v.Type == traceql.TypeString {
		s := v.EncodeToString(false)
		if !strings.Contains(s, "`") {
			return "`" + s + "`"
		}
		return strconv.Quote(s)
	}
	return v.EncodeToString(true)
}

func scopedName(scope traceql.AttributeScope, name string) string {
	return scope.String() + "." + strconv.Quote(name)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// TestTagsVsVP5 compares every tag-search API of the CG block (CG_BLOCK_DIR) with its vParquet5
// source (CG_VP5_DIR): the results must be the same sets. CG_TAG_CAP caps the attribute keys
// per level whose values are compared (default 40); CG_START_PAGE/CG_PAGES select a shard.
// Mismatches of the filtered calls are checked against a TraceQL search on the vParquet5
// block (ground truth: does a span with the conditions and the value exist?).
func TestTagsVsVP5(t *testing.T) {
	cgDir, vpDir := os.Getenv("CG_BLOCK_DIR"), os.Getenv("CG_VP5_DIR")
	if cgDir == "" || vpDir == "" {
		t.Skip("CG_BLOCK_DIR / CG_VP5_DIR not set")
	}
	ctx := context.Background()
	b := &tagBlocks{opts: common.DefaultSearchOptions()}
	b.cg, b.cgR = openTagBlock(t, cgDir)
	b.vp, b.vpR = openTagBlock(t, vpDir)
	b.opts.StartPage, b.opts.TotalPages = atoiT(os.Getenv("CG_START_PAGE")), atoiT(os.Getenv("CG_PAGES"))
	keyCap := atoiT(envOr("CG_TAG_CAP", "40"))
	c := &tagCompare{t: t, byKind: map[string]int{}, explain: map[string]string{}}
	nop := func(uint64) {}

	// ---- SearchTags, every scope ----
	scopes := []traceql.AttributeScope{traceql.AttributeScopeNone, traceql.AttributeScopeResource, traceql.AttributeScopeInstrumentation,
		traceql.AttributeScopeSpan, traceql.AttributeScopeEvent, traceql.AttributeScopeLink, traceql.AttributeScopeTrace}
	keysByScope := map[traceql.AttributeScope][]string{}
	for _, sc := range scopes {
		cgSet, vpSet, cgIO, vpIO, cgD, vpD, err := b.run(func(blk common.BackendBlock, s *vset) error {
			return blk.SearchTags(ctx, sc, func(n string, sc traceql.AttributeScope) { s.addKey(sc.String() + "|" + n) }, nop, b.opts)
		})
		require.NoError(t, err)
		if sc != traceql.AttributeScopeNone {
			for _, k := range vpSet.ex {
				keysByScope[sc] = append(keysByScope[sc], strings.SplitN(k, "|", 2)[1])
			}
			sort.Strings(keysByScope[sc])
		}
		c.compare("SearchTags", sc.String(), cgSet, vpSet, cgIO, vpIO, cgD, vpD)
	}

	// ---- SearchTagValuesV2: every intrinsic, attribute keys of every level ----
	var tags []traceql.Attribute
	for _, in := range []traceql.Intrinsic{traceql.IntrinsicName, traceql.IntrinsicStatus, traceql.IntrinsicStatusMessage, traceql.IntrinsicDuration,
		traceql.IntrinsicKind, traceql.IntrinsicSpanID, traceql.IntrinsicParentID, traceql.IntrinsicSpanStartTime, traceql.IntrinsicNestedSetLeft,
		traceql.IntrinsicNestedSetRight, traceql.IntrinsicNestedSetParent, traceql.IntrinsicChildCount, traceql.IntrinsicSpanMultiplier,
		traceql.IntrinsicStructuralDescendant, traceql.IntrinsicTraceRootService, traceql.IntrinsicTraceRootSpan, traceql.IntrinsicTraceDuration,
		traceql.IntrinsicTraceID, traceql.IntrinsicTraceStartTime, traceql.IntrinsicServiceStats, traceql.IntrinsicEventName,
		traceql.IntrinsicEventTimeSinceStart, traceql.IntrinsicLinkTraceID, traceql.IntrinsicLinkSpanID, traceql.IntrinsicInstrumentationName,
		traceql.IntrinsicInstrumentationVersion} {
		tags = append(tags, traceql.NewIntrinsic(in))
	}
	dedicated := map[string]bool{}
	for _, d := range b.vp.BlockMeta().DedicatedColumns {
		dedicated[string(d.Scope)+"/"+d.Name] = true
	}
	dedScope := map[traceql.AttributeScope]string{traceql.AttributeScopeSpan: "span", traceql.AttributeScopeResource: "resource", traceql.AttributeScopeEvent: "event"}
	for _, sc := range []traceql.AttributeScope{traceql.AttributeScopeResource, traceql.AttributeScopeSpan, traceql.AttributeScopeEvent,
		traceql.AttributeScopeLink, traceql.AttributeScopeInstrumentation} {
		keys := keysByScope[sc]
		step := max(1, len(keys)/max(1, keyCap))
		for i, k := range keys {
			if i%step == 0 || dedicated[dedScope[sc]+"/"+k] {
				tags = append(tags, traceql.NewScopedAttribute(sc, false, k))
			}
		}
	}
	for _, k := range []string{"service.name", "http.method", "http.route", "cluster", "http.status_code", "exception.type", "db.system"} {
		tags = append(tags, traceql.NewAttribute(k))
	}
	for _, tag := range tags {
		cgSet, vpSet, cgIO, vpIO, cgD, vpD, err := b.run(func(blk common.BackendBlock, s *vset) error {
			return blk.SearchTagValuesV2(ctx, tag, func(v traceql.Static) bool { s.add(v); return false }, nop, b.opts)
		})
		require.NoError(t, err)
		c.compare("TagValuesV2", tag.String(), cgSet, vpSet, cgIO, vpIO, cgD, vpD)
	}

	// ---- SearchTagValues (v1 names) ----
	for _, tag := range []string{"name", "status.code", "service.name", "http.method", "http.url", "root.service.name", "root.name",
		"rootServiceName", "traceID", "cluster", "namespace", "k8s.namespace.name", "http.status_code", "db.system"} {
		cgSet, vpSet, cgIO, vpIO, cgD, vpD, err := b.run(func(blk common.BackendBlock, s *vset) error {
			return blk.SearchTagValues(ctx, tag, func(v string) bool { s.addKey(v); return false }, nop, b.opts)
		})
		require.NoError(t, err)
		c.compare("TagValuesV1", tag, cgSet, vpSet, cgIO, vpIO, cgD, vpD)
	}

	// ---- FetchTagNames / FetchTagValues with conditions ----
	queries := strings.Split(envOr("CG_TAG_QUERIES", strings.Join([]string{
		"resource.service.name = `smtprelay`",
		"span.http.method = `GET`",
		"duration > 1s",
		"resource.service.name = `storage-api` && span.http.method = `GET`",
		"status = error && kind = server",
		"span.http.status_code >= 400",
		"resource.service.name = `CockroachDB` && event:name = `cockroachdb-structured-event` || span.http.route != nil",
	}, ";")), ";")
	groups := func(q string) [][]traceql.Condition {
		g, err := traceql.ExtractConditionGroups("{ "+q+" }", 100)
		require.NoError(t, err)
		require.NotEmpty(t, g, q)
		return g
	}
	var vp5Bug []string
	for _, q := range queries {
		union := newVset()
		var none *vset
		for _, sc := range scopes[:6] {
			req := traceql.FetchTagsRequest{ConditionGroups: groups(q), Scope: sc}
			cgSet, vpSet, cgIO, vpIO, cgD, vpD, err := b.run(func(blk common.BackendBlock, s *vset) error {
				return blk.FetchTagNames(ctx, req, func(n string, sc traceql.AttributeScope) bool { s.addKey(sc.String() + "|" + n); return false }, nop, b.opts)
			})
			require.NoError(t, err)
			name := fmt.Sprintf("{ %s } scope=%s", q, sc)
			onlyCG, onlyVP := c.compare("FetchTagNames", name, cgSet, vpSet, cgIO, vpIO, cgD, vpD)
			if sc == traceql.AttributeScopeNone {
				none = vpSet
			} else {
				for h, k := range vpSet.ex {
					union.h = append(union.h, h)
					union.ex[h] = k
				}
			}
			c.groundTruthNames(ctx, b, q, name, cgSet, onlyCG, vpSet, onlyVP)
		}
		union.finish()
		if a, bb := diff(union, none); len(a)+len(bb) > 0 {
			vp5Bug = append(vp5Bug, fmt.Sprintf("{ %s }: vP5 scope=none misses %d names its per-scope calls return %v (and has %d extra)", q, len(a), union.examples(a, 5), len(bb)))
		}
	}
	valueTags := []string{"resource.service.name", "span.http.method", "span.http.status_code", "span.http.route", "resource.cluster",
		"resource.k8s.namespace.name", "name", "status", "kind", "duration", "statusMessage", "rootServiceName", "rootName", "traceDuration",
		"instrumentation:name", "instrumentation:version", "event:name", "event.exception.type", "event.level", "link.opentracing.ref_type",
		".service.name", ".http.method", "span.db.system", "resource.telemetry.sdk.language", "span:id", "nestedSetLeft", "span:childCount", "link:traceID"}
	for _, q := range queries {
		for _, ts := range valueTags {
			tag, err := traceql.ParseIdentifier(ts)
			require.NoError(t, err, ts)
			cgSet, vpSet, cgIO, vpIO, cgD, vpD, err := b.run(func(blk common.BackendBlock, s *vset) error {
				f := traceql.NewTagValuesFetcherWrapper(func(ctx context.Context, req traceql.FetchTagValuesRequest, cb traceql.FetchTagValuesCallback) error {
					return blk.FetchTagValues(ctx, req, cb, nop, b.opts)
				})
				return traceql.NewEngine().ExecuteTagValues(ctx, tag, groups(q), func(v traceql.Static) bool { s.add(v); return false }, f, 100)
			})
			require.NoError(t, err)
			name := fmt.Sprintf("%s { %s }", ts, q)
			onlyCG, onlyVP := c.compare("FetchTagValues", name, cgSet, vpSet, cgIO, vpIO, cgD, vpD)
			c.groundTruthValues(ctx, b, q, ts, name, cgSet, onlyCG, vpSet, onlyVP)
		}
	}

	t.Logf("==== I/O (requests / bytes) and time per call ====")
	for _, l := range c.io {
		t.Log(l)
	}
	t.Logf("==== %d calls compared %v; %d mismatched ====", c.calls, c.byKind, len(c.bad))
	for _, m := range c.bad {
		t.Log("  " + m)
		if e := c.explain[m]; e != "" {
			t.Log("    ground truth: " + e)
		}
	}
	for _, m := range vp5Bug {
		t.Log("vP5 inconsistency: " + m)
	}
}

func atoiT(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// groundTruthNames checks a few names only one side reports with a vParquet5 search for a
// span meeting the conditions whose owner at that scope has the attribute.
func (c *tagCompare) groundTruthNames(ctx context.Context, b *tagBlocks, q, name string, cgSet *vset, onlyCG []uint64, vpSet *vset, onlyVP []uint64) {
	if len(onlyCG)+len(onlyVP) == 0 {
		return
	}
	check := func(s *vset, hs []uint64, side string) string {
		var out []string
		for _, h := range hs[:min(3, len(hs))] {
			k, ok := s.ex[h]
			if !ok {
				continue
			}
			parts := strings.SplitN(k, "|", 2)
			gq := fmt.Sprintf("{ (%s) && %s.%s != nil }", q, parts[0], strconv.Quote(parts[1]))
			found, err := exists(ctx, b.vp, b.opts, gq)
			out = append(out, fmt.Sprintf("%s %s exists=%v err=%v", side, gq, found, err))
		}
		return strings.Join(out, "; ")
	}
	c.explain[c.bad[len(c.bad)-1]] = check(cgSet, onlyCG, "onlyCG") + " | " + check(vpSet, onlyVP, "onlyVP5")
	_ = name
}

// groundTruthValues checks a few values only one side reports with a vParquet5 search for a
// span meeting the conditions and having the value.
func (c *tagCompare) groundTruthValues(ctx context.Context, b *tagBlocks, q, tag, name string, cgSet *vset, onlyCG []uint64, vpSet *vset, onlyVP []uint64) {
	if len(onlyCG)+len(onlyVP) == 0 {
		return
	}
	check := func(s *vset, hs []uint64, side string) string {
		var out []string
		for _, h := range hs[:min(3, len(hs))] {
			v, ok := s.st[h]
			if !ok || v.Type == traceql.TypeNil {
				continue
			}
			gq := fmt.Sprintf("{ (%s) && %s = %s }", q, tag, literal(v))
			found, err := exists(ctx, b.vp, b.opts, gq)
			out = append(out, fmt.Sprintf("%s %s exists=%v err=%v", side, gq, found, err))
		}
		return strings.Join(out, "; ")
	}
	c.explain[c.bad[len(c.bad)-1]] = check(cgSet, onlyCG, "onlyCG") + " | " + check(vpSet, onlyVP, "onlyVP5")
	_ = name
}

// TestTagsVP5DurationGroundTruth shows the vParquet5 duration-predicate bug behind the
// `duration > 1s` mismatches of TestTagsVsVP5: spans longer than 1s that vParquet5's own
// `duration > 1s` search (and autocomplete) skip. Names come from that comparison's output.
func TestTagsVP5DurationGroundTruth(t *testing.T) {
	vpDir := os.Getenv("CG_VP5_DIR")
	if vpDir == "" {
		t.Skip("CG_VP5_DIR not set")
	}
	vp, _ := openTagBlock(t, vpDir)
	ctx := context.Background()
	opts := common.DefaultSearchOptions()
	search := func(q string) *tempopb.SearchResponse {
		f := traceql.NewSpansetFetcherWrapper(func(ctx context.Context, req traceql.FetchSpansRequest) (traceql.FetchSpansResponse, error) {
			return vp.Fetch(ctx, req, opts)
		})
		resp, err := traceql.NewEngine().ExecuteSearch(ctx, &tempopb.SearchRequest{Query: q, Limit: 100000, SpansPerSpanSet: 100000}, f)
		require.NoError(t, err)
		return resp
	}
	for _, name := range []string{"Auto-merge Atlantis Automation PRs", "Assistant - Remove Plugin After Kube Manifests Export"} {
		resp := search("{ name = `" + name + "` }")
		n, long := 0, 0
		for _, tr := range resp.Traces {
			for _, ss := range tr.SpanSets {
				for _, sp := range ss.Spans {
					n++
					if sp.DurationNanos > 1e9 {
						long++
						if long <= 2 {
							t.Logf("%q: trace %s span %s duration %dns (> 1s) rootName=%q traceDur=%dms", name, tr.TraceID, sp.SpanID, sp.DurationNanos, tr.RootTraceName, tr.DurationMs)
						}
					}
				}
			}
		}
		t.Logf("%q: %d spans, %d longer than 1s; vP5 `duration > 1s && name = ...` finds %d traces", name, n, long, len(search("{ duration > 1s && name = `"+name+"` }").Traces))
	}
}
