package cgfile

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// Tests on a real block, skipped unless CG_BLOCK names a block.cg file:
//
//	CG_BLOCK=/path/block.cg                      the CG block
//	CG_VP5_ROOT=/path/tempo-large-block          local backend root of its vParquet5 source
//	CG_VP5_TENANT=3  CG_VP5_BLOCK=<uuid>         (default: tenant 3, its only block)
//	CG_SAMPLE_IDS=/path/sample_ids.json          [{"id": "<hex>"}, ...] or ["<hex>", ...]

type fileReader struct{ f *os.File }

func (r fileReader) ReadRange(_ context.Context, off int64, buf []byte) error {
	_, err := r.f.ReadAt(buf, off)
	return err
}

func openTestBlock(t testing.TB) *Reader {
	path := os.Getenv("CG_BLOCK")
	if path == "" {
		t.Skip("CG_BLOCK not set")
	}
	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	st, err := f.Stat()
	require.NoError(t, err)
	r, err := Open(context.Background(), fileReader{f}, st.Size())
	require.NoError(t, err)
	t.Cleanup(r.Close)
	return r
}

func TestBlockFooter(t *testing.T) {
	r := openTestBlock(t)
	q, err := r.NewRequest(context.Background())
	require.NoError(t, err)
	require.Equal(t, Stats{Requests: 1, Bytes: int64(r.FooterLen()), Waves: 1}, q.Stats())
	f := q.F
	chunks, pages, runs := 0, 0, 0
	for tb := 0; tb < NumTables; tb++ {
		for gi := range f.Groups[tb] {
			g, err := q.Group(tb, gi)
			require.NoError(t, err)
			chunks += len(g.Chunks)
			pages += len(g.Pages)
			var rows uint32
			for _, p := range g.Pages {
				require.Equal(t, rows, p.First)
				rows += p.Rows
			}
			require.Equal(t, f.Rows[tb], rows, "%s group %s", TableName(tb), f.Groups[tb][gi].Name)
		}
	}
	for l := 0; l < NumLevels; l++ {
		km, err := q.Km(l)
		require.NoError(t, err)
		var rows uint32
		for _, k := range km {
			runs += len(k.Runs)
			for _, r := range k.Runs {
				rows += r.Rows
			}
		}
		require.Equal(t, f.Rows[KmTable(l)], rows)
	}
	t.Logf("footer %d bytes, %d anchors, %d chunks, %d pages, %d runs, keys %d/%d/%d/%d/%d",
		r.FooterLen(), len(f.Anchors), chunks, pages, runs, len(f.Keys[0]), len(f.Keys[1]), len(f.Keys[2]), len(f.Keys[3]), len(f.Keys[4]))
	for i := range f.Anchors {
		g, err := q.Group(TTraces, mustGroup(t, f, TTraces, TrFirstSpan))
		require.NoError(t, err)
		require.Equal(t, g.Pages[i].First, f.Anchors[i][TTraces])
	}
}

func mustGroup(t testing.TB, f *Footer, tb, c int) int {
	gi, _, ok := f.ColumnGroup(tb, c)
	require.True(t, ok)
	return gi
}

// TestBlockDecodeAllPages decodes every page of every column of several tables (a fresh
// request per column to bound memory) and checks rows and nulls against the footer.
func TestBlockDecodeAllPages(t *testing.T) {
	r := openTestBlock(t)
	ctx := context.Background()
	q0, err := r.NewRequest(ctx)
	require.NoError(t, err)
	tables := []int{TTraces, TServiceStats, TResources, TScopes, TLinks,
		AttrsTable(LScope), AttrsTable(LLink), ElemsTable(LSpan), ElemsTable(LEvent), ElemsTable(LResource)}
	if os.Getenv("CG_ALL_TABLES") != "" {
		tables = tables[:0]
		for tb := 0; tb < NumTables; tb++ {
			if len(q0.F.Groups[tb]) > 0 {
				tables = append(tables, tb)
			}
		}
	}
	for _, tb := range tables {
		for _, c := range StoredCols(tb) {
			q := r.NewRequestWithFooter(q0.F)
			col, err := q.Column(tb, c)
			require.NoError(t, err)
			all := make([]int, col.NumPages())
			for i := range all {
				all[i] = i
			}
			require.NoError(t, q.Load(ctx, []PageSel{{T: tb, C: c, Pages: all}}, nil), "%s.%s", TableName(tb), col.Spec.Name)
			var rows uint32
			for i := range all {
				v, p := col.Values(i), col.Page(i)
				require.Equal(t, int(p.Rows), v.N)
				nulls := 0
				for j := range v.N {
					if v.IsNull(j) {
						nulls++
					} else if v.Dict {
						require.Less(t, int(v.Codes[j]), col.Dict().Len())
					}
				}
				require.Equal(t, int(col.Buf(i).Nulls), nulls)
				if z := col.Zone(i); z.Kind == ZoneInt && nulls < v.N {
					mn, mx := int64(1<<63-1), int64(-1<<63)
					for j := range v.N {
						if !v.IsNull(j) {
							mn, mx = min(mn, v.Ints[j]), max(mx, v.Ints[j])
						}
					}
					require.Equal(t, [2]int64{z.Min, z.Max}, [2]int64{mn, mx}, "%s.%s page %d zone", TableName(tb), col.Spec.Name, i)
				}
				rows += uint32(v.N)
			}
			require.Equal(t, q.Rows(tb), rows)
			// a merged dictionary holds each value once
			seen := make(map[string]struct{}, col.Dict().Len())
			for _, v := range col.Dict().Values {
				_, dup := seen[string(v)]
				require.False(t, dup, "%s.%s: duplicate dictionary value", TableName(tb), col.Spec.Name)
				seen[string(v)] = struct{}{}
			}
			t.Logf("%s.%s: %d pages, %d dict entries, %+v", TableName(tb), col.Spec.Name, len(all), col.Dict().Len(), q.Stats())
		}
	}
}

// TestBlockRuns decodes every run of the scope and link levels and checks owners and links.
func TestBlockRuns(t *testing.T) {
	r := openTestBlock(t)
	ctx := context.Background()
	q0, err := r.NewRequest(ctx)
	require.NoError(t, err)
	for _, l := range []int{LScope, LLink, LResource} {
		q := r.NewRequestWithFooter(q0.F)
		km, err := q.Km(l)
		require.NoError(t, err)
		var refs []RunRef
		for fi := range km {
			for ri := range km[fi].Runs {
				refs = append(refs, RunRef{l, fi, ri})
			}
		}
		require.NoError(t, q.Load(ctx, nil, refs))
		for _, ref := range refs {
			rd := q.Run(ref)
			k := km[ref.Flush]
			require.Len(t, rd.Owners, int(rd.Meta.Rows))
			for i, o := range rd.Owners {
				require.True(t, o >= k.Owner0 && o < k.Owner0+k.Owners)
				if i > 0 {
					require.LessOrEqual(t, rd.Owners[i-1], o)
				}
			}
			if l != LSpan {
				require.Len(t, rd.Links, map[int]int{LResource: 2, LScope: 2, LEvent: 1, LLink: 1}[l])
				for _, s := range rd.Links[0] {
					require.Less(t, s, q.Rows(TSpans))
				}
			}
			if rd.Vals != nil && rd.Meta.Zone.Kind == ZoneInt {
				for _, x := range rd.Vals.Ints {
					require.True(t, x >= rd.Meta.Zone.Min && x <= rd.Meta.Zone.Max)
				}
			}
		}
		id, ok := q.F.KeyID(l, q.F.Keys[l][0])
		require.True(t, ok)
		kr, err := q.KeyRuns(l, id, 0, q.Rows(OwnerTable[l]))
		require.NoError(t, err)
		require.NotEmpty(t, kr)
		t.Logf("level %d: %d runs, key %q has %d runs, %+v", l, len(refs), q.F.Keys[l][0], len(kr), q.Stats())
	}
}
