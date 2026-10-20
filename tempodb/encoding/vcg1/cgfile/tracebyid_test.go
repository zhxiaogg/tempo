package cgfile

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/gogo/protobuf/proto"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	v1 "github.com/grafana/tempo/v3/pkg/tempopb/common/v1"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/backend/local"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

func sampleIDs(t *testing.T) [][]byte {
	path := os.Getenv("CG_SAMPLE_IDS")
	if path == "" {
		t.Skip("CG_SAMPLE_IDS not set")
	}
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var objs []struct {
		ID string `json:"id"`
	}
	var strs []string
	if json.Unmarshal(b, &objs) != nil {
		require.NoError(t, json.Unmarshal(b, &strs))
	}
	for _, o := range objs {
		strs = append(strs, o.ID)
	}
	out := make([][]byte, 0, len(strs))
	for _, s := range strs {
		id, err := hex.DecodeString(s)
		require.NoError(t, err)
		out = append(out, id)
	}
	return out
}

func openVP5(t *testing.T) common.BackendBlock {
	root := os.Getenv("CG_VP5_ROOT")
	if root == "" {
		t.Skip("CG_VP5_ROOT not set")
	}
	tenant := os.Getenv("CG_VP5_TENANT")
	if tenant == "" {
		tenant = "3"
	}
	raw, _, _, err := local.New(&local.Config{Path: root})
	require.NoError(t, err)
	r := backend.NewReader(raw)
	ctx := context.Background()
	var id uuid.UUID
	if s := os.Getenv("CG_VP5_BLOCK"); s != "" {
		id = uuid.MustParse(s)
	} else {
		blocks, _, _, err := r.Blocks(ctx, tenant)
		require.NoError(t, err)
		require.Len(t, blocks, 1)
		id = blocks[0]
	}
	meta, err := r.BlockMeta(ctx, id, tenant)
	require.NoError(t, err)
	blk, err := vparquet5.Encoding{}.OpenBlock(meta, newBloomPass(t, r))
	require.NoError(t, err)
	return blk
}

// bloomPass answers bloom reads with a filter that admits every id: the test block has no
// bloom objects (same approach as cmd/zz-querybench).
type bloomPass struct {
	backend.Reader
	bloom []byte
}

func newBloomPass(t *testing.T, r backend.Reader) backend.Reader {
	f := bloom.New(1, 1)
	f.Add([]byte{0})
	var buf bytes.Buffer
	_, err := f.WriteTo(&buf)
	require.NoError(t, err)
	return &bloomPass{Reader: r, bloom: buf.Bytes()}
}

func (r *bloomPass) Read(ctx context.Context, name string, id uuid.UUID, tenant string, ci *backend.CacheInfo) ([]byte, error) {
	if strings.HasPrefix(name, "bloom-") {
		return r.bloom, nil
	}
	return r.Reader.Read(ctx, name, id, tenant, ci)
}

func TestFindTraceByIDvsVP5(t *testing.T) {
	r := openTestBlock(t)
	ids := sampleIDs(t)
	all := compareIDs(t, r, openVP5(t), ids)
	logStats(t, all)
}

// compareIDs checks FindTraceByID against vparquet5 for every id: the marshaled traces must
// be equal after sorting attribute lists (and are counted when equal as returned).
func compareIDs(t *testing.T, r *Reader, vp common.BackendBlock, ids [][]byte) []Stats {
	ctx := context.Background()
	var all []Stats
	exact, canonical := 0, 0
	for _, id := range ids {
		got, st, err := r.FindTraceByID(ctx, id)
		require.NoError(t, err, "%x", id)
		all = append(all, st)
		resp, err := vp.FindTraceByID(ctx, id, common.DefaultSearchOptions())
		require.NoError(t, err)
		want := resp.Trace
		require.Equal(t, want == nil, got == nil, "%x found", id)
		if want == nil {
			exact++
			canonical++
			continue
		}
		if bytes.Equal(marshal(t, got), marshal(t, want)) {
			exact++
		}
		sortAttrs(got)
		sortAttrs(want)
		if !bytes.Equal(marshal(t, got), marshal(t, want)) {
			reportDiff(t, id, got, want)
			continue
		}
		canonical++
	}
	t.Logf("%d ids: %d equal as returned, %d equal after sorting attributes", len(ids), exact, canonical)
	require.Equal(t, len(ids), canonical)
	return all
}

func logStats(t *testing.T, all []Stats) {
	med := func(f func(Stats) int64) int64 {
		v := make([]int64, len(all))
		for i, s := range all {
			v[i] = f(s)
		}
		slices.Sort(v)
		return v[len(v)/2]
	}
	var sum Stats
	for _, s := range all {
		sum.Add(s)
	}
	n := int64(len(all))
	t.Logf("per lookup: median %d requests / %d bytes / %d waves; mean %d requests / %d bytes / %.2f waves",
		med(func(s Stats) int64 { return s.Requests }), med(func(s Stats) int64 { return s.Bytes }),
		med(func(s Stats) int64 { return s.Waves }), sum.Requests/n, sum.Bytes/n, float64(sum.Waves)/float64(n))
}

func marshal(t *testing.T, tr *tempopb.Trace) []byte {
	b, err := tr.Marshal()
	require.NoError(t, err)
	return b
}

// sortAttrs orders every attribute list by key, then value (vparquet5 emits generic
// attributes, then dedicated columns, then service.name; CG keeps one generic list).
func sortAttrs(tr *tempopb.Trace) {
	for _, rs := range tr.ResourceSpans {
		sortKVs(rs.Resource.Attributes)
		for _, ss := range rs.ScopeSpans {
			sortKVs(ss.Scope.Attributes)
			for _, s := range ss.Spans {
				sortKVs(s.Attributes)
				for _, e := range s.Events {
					sortKVs(e.Attributes)
				}
				for _, l := range s.Links {
					sortKVs(l.Attributes)
				}
			}
		}
	}
}

func sortKVs(kvs []*v1.KeyValue) {
	sort.SliceStable(kvs, func(i, j int) bool {
		if kvs[i].Key != kvs[j].Key {
			return kvs[i].Key < kvs[j].Key
		}
		a, _ := kvs[i].Value.Marshal()
		b, _ := kvs[j].Value.Marshal()
		return bytes.Compare(a, b) < 0
	})
}

func reportDiff(t *testing.T, id []byte, got, want *tempopb.Trace) {
	t.Helper()
	m := jsonpb.Marshaler{}
	js := func(v proto.Message) string {
		s, _ := m.MarshalToString(v)
		return s
	}
	if len(got.ResourceSpans) != len(want.ResourceSpans) {
		t.Errorf("%x: %d resource spans, want %d", id, len(got.ResourceSpans), len(want.ResourceSpans))
		return
	}
	for i := range got.ResourceSpans {
		g, w := got.ResourceSpans[i], want.ResourceSpans[i]
		if a, b := js(g.Resource), js(w.Resource); a != b {
			t.Errorf("%x resource %d:\n got %s\nwant %s", id, i, a, b)
			return
		}
		if len(g.ScopeSpans) != len(w.ScopeSpans) {
			t.Errorf("%x resource %d: %d scopes, want %d", id, i, len(g.ScopeSpans), len(w.ScopeSpans))
			return
		}
		for j := range g.ScopeSpans {
			gs, ws := g.ScopeSpans[j], w.ScopeSpans[j]
			if a, b := js(gs.Scope), js(ws.Scope); a != b {
				t.Errorf("%x scope %d/%d:\n got %s\nwant %s", id, i, j, a, b)
				return
			}
			if len(gs.Spans) != len(ws.Spans) {
				t.Errorf("%x scope %d/%d: %d spans, want %d", id, i, j, len(gs.Spans), len(ws.Spans))
				return
			}
			for k := range gs.Spans {
				if a, b := js(gs.Spans[k]), js(ws.Spans[k]); a != b {
					t.Errorf("%x span %d/%d/%d:\n got %s\nwant %s", id, i, j, k, a, b)
					return
				}
			}
		}
	}
	t.Errorf("%x: traces differ (no field-level difference found)", id)
}

func BenchmarkFindTraceByID(b *testing.B) {
	r := openTestBlock(b)
	path := os.Getenv("CG_SAMPLE_IDS")
	if path == "" {
		b.Skip("CG_SAMPLE_IDS not set")
	}
	raw, err := os.ReadFile(path)
	require.NoError(b, err)
	var objs []struct {
		ID string `json:"id"`
	}
	require.NoError(b, json.Unmarshal(raw, &objs))
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id, _ := hex.DecodeString(objs[i%len(objs)].ID)
		if _, _, err := r.FindTraceByID(ctx, id); err != nil {
			b.Fatal(err)
		}
	}
}
