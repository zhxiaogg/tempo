package cgwrite_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/v3/pkg/util"
	"github.com/grafana/tempo/v3/tempodb/backend"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgwrite"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1/internal/cgtest"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

type memRR []byte

func (m memRR) ReadRange(_ context.Context, off int64, b []byte) error {
	copy(b, m[off:])
	return nil
}

// write writes traces with opts (cutting an anchor every cutEvery traces when > 0) and returns
// the file, the result and each trace's expected OTLP form (vParquet5's own conversion).
func write(t *testing.T, traces []cgtest.Trace, opts cgwrite.Options, cutEvery int) ([]byte, cgwrite.Result, [][]byte) {
	var out bytes.Buffer
	w, err := cgwrite.New(&out, opts)
	require.NoError(t, err)
	conv := vparquet5.NewTraceConverter(opts.Dedicated)
	meta := &backend.BlockMeta{DedicatedColumns: opts.Dedicated}
	want := make([][]byte, len(traces))
	for i, tr := range traces {
		row, _ := conv.Convert(tr.ID, tr.Trace, nil)
		if cutEvery > 0 && i > 0 && i%cutEvery == 0 {
			w.CutAnchor()
		}
		require.NoError(t, w.Add(row))
		want[i] = cgtest.Canon(vparquet5.ParquetTraceToTempopbTrace(meta, row))
	}
	res, err := w.Close()
	require.NoError(t, err)
	require.Equal(t, uint64(out.Len()), res.Size)
	return out.Bytes(), res, want
}

func TestRoundTrip(t *testing.T) {
	traces := cgtest.Traces(3000, 1)
	for _, tc := range []struct {
		name string
		opts cgwrite.Options
	}{
		{"defaults", cgwrite.Options{}},
		// tiny limits: many pages, chunks, dictionary fallbacks and scan-copy flushes
		{"tiny", cgwrite.Options{PageBytes: 2 << 10, AttrPageBytes: 8 << 10, ChunkBytes: 16 << 10, KmBytes: 64 << 10, PartBytes: 4 << 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Dedicated = cgtest.Dedicated
			file, res, want := write(t, traces, tc.opts, 0)
			require.Equal(t, uint32(len(traces)), res.Traces)

			ctx := context.Background()
			rd, err := cg.OpenKnown(memRR(file), int64(len(file)), res.FooterLen)
			require.NoError(t, err)
			defer rd.Close()
			q, err := rd.NewRequest(ctx)
			require.NoError(t, err)
			f := q.F
			require.Len(t, f.Anchors, res.Anchors)
			require.Equal(t, res.Rows, f.Rows)
			// anchors start at trace rows, ascending
			for i := 1; i < len(f.Anchors); i++ {
				for tb := 0; tb < cg.NumTables; tb++ {
					require.LessOrEqual(t, f.Anchors[i-1][tb], f.Anchors[i][tb])
				}
			}
			// key-major flushes tile their owner tables in order
			for l := 0; l < cg.NumLevels; l++ {
				km, err := q.Km(l)
				require.NoError(t, err)
				next := uint32(0)
				for _, fl := range km {
					require.Equal(t, next, fl.Owner0)
					next = fl.Owner0 + fl.Owners
				}
				require.LessOrEqual(t, next, f.Rows[cg.OwnerTable[l]])
			}
			if tc.name == "tiny" {
				require.Greater(t, res.Chunks, 100)
				for l := range res.KmFlushes {
					if f.Rows[cg.KmTable(l)] > 2000 {
						require.Greater(t, res.KmFlushes[l], 1, "level %d", l)
					}
				}
			}

			for i, tr := range traces {
				got, _, err := rd.FindTraceByID(ctx, util.PadTraceIDTo16Bytes(tr.ID))
				require.NoError(t, err)
				require.NotNil(t, got, "trace %d", i)
				require.Equal(t, want[i], cgtest.Canon(got), "trace %d %x", i, tr.ID)
			}
			got, _, err := rd.FindTraceByID(ctx, bytes.Repeat([]byte{0xff}, 16))
			require.NoError(t, err)
			require.Nil(t, got)
		})
	}
}

func TestCutAnchor(t *testing.T) {
	traces := cgtest.Traces(1000, 2)
	file, res, _ := write(t, traces, cgwrite.Options{Dedicated: cgtest.Dedicated}, 100)
	rd, err := cg.OpenKnown(memRR(file), int64(len(file)), res.FooterLen)
	require.NoError(t, err)
	q, err := rd.NewRequest(context.Background())
	require.NoError(t, err)
	starts := map[uint32]bool{}
	for _, a := range q.F.Anchors {
		starts[a[cg.TTraces]] = true
	}
	for i := 0; i < len(traces); i += 100 {
		require.True(t, starts[uint32(i)], "an anchor starts at trace %d", i)
	}
}

func TestOrder(t *testing.T) {
	traces := cgtest.Traces(2, 3)
	w, err := cgwrite.New(&bytes.Buffer{}, cgwrite.Options{})
	require.NoError(t, err)
	conv := vparquet5.NewTraceConverter(nil)
	row, _ := conv.Convert(traces[1].ID, traces[1].Trace, nil)
	require.NoError(t, w.Add(row))
	row, _ = conv.Convert(traces[0].ID, traces[0].Trace, nil)
	require.Error(t, w.Add(row))
}

// TestColumns checks the stored columns trace by id does not return (trace-level fields,
// service stats, nested set, rounded start times) against the vParquet5 rows.
func TestColumns(t *testing.T) {
	traces := cgtest.Traces(3000, 1)
	for _, opts := range []cgwrite.Options{{}, {PageBytes: 2 << 10, AttrPageBytes: 8 << 10, ChunkBytes: 16 << 10, KmBytes: 64 << 10}} {
		opts.Dedicated = cgtest.Dedicated
		file, res, _ := write(t, traces, opts, 0)
		ctx := context.Background()
		rd, err := cg.OpenKnown(memRR(file), int64(len(file)), res.FooterLen)
		require.NoError(t, err)
		q, err := rd.NewRequest(ctx)
		require.NoError(t, err)
		var sels []cg.RowSel
		for _, tb := range []int{cg.TTraces, cg.TServiceStats, cg.TSpans} {
			sels = append(sels, cg.RowSel{T: tb, Ivs: []cg.Interval{{Lo: 0, Hi: q.Rows(tb)}}})
		}
		require.NoError(t, q.LoadRows(ctx, sels))
		get := func(tb, c int, r uint32) int64 {
			col, err := q.Column(tb, c)
			require.NoError(t, err)
			v, _, err := col.Int(r)
			require.NoError(t, err)
			return v
		}
		str := func(tb, c int, r uint32) string {
			col, err := q.Column(tb, c)
			require.NoError(t, err)
			b, _, err := col.Bytes(r)
			require.NoError(t, err)
			return string(b)
		}
		conv := vparquet5.NewTraceConverter(opts.Dedicated)
		var ss, sp uint32
		for i, tr := range traces {
			row, _ := conv.Convert(tr.ID, tr.Trace, nil)
			r := uint32(i)
			require.Equal(t, int64(row.StartTimeUnixNano), get(cg.TTraces, cg.TrStart, r))
			require.Equal(t, int64(row.EndTimeUnixNano), get(cg.TTraces, cg.TrEnd, r))
			require.Equal(t, int64(row.DurationNano), get(cg.TTraces, cg.TrDuration, r))
			require.Equal(t, row.RootServiceName, str(cg.TTraces, cg.TrRootService, r))
			require.Equal(t, row.RootSpanName, str(cg.TTraces, cg.TrRootSpan, r))
			for _, s := range row.ServiceStats {
				require.Equal(t, s.ServiceName, str(cg.TServiceStats, cg.SsServiceName, ss))
				require.Equal(t, int64(s.SpanCount), get(cg.TServiceStats, cg.SsSpanCount, ss))
				require.Equal(t, int64(s.ErrorCount), get(cg.TServiceStats, cg.SsErrorCount, ss))
				ss++
			}
			for _, rs := range row.ResourceSpans {
				for _, sc := range rs.ScopeSpans {
					for _, s := range sc.Spans {
						require.Equal(t, int64(s.ParentID), get(cg.TSpans, cg.SpParentID, sp))
						require.Equal(t, int64(s.NestedSetLeft), get(cg.TSpans, cg.SpNestedLeft, sp))
						require.Equal(t, int64(s.NestedSetRight), get(cg.TSpans, cg.SpNestedRight, sp))
						require.Equal(t, int64(s.ChildCount), get(cg.TSpans, cg.SpChildCount, sp))
						require.Equal(t, int64(int32(s.StartTimeRounded15)), get(cg.TSpans, cg.SpRounded15, sp))
						require.Equal(t, int64(int32(s.StartTimeRounded60)), get(cg.TSpans, cg.SpRounded60, sp))
						require.Equal(t, int64(int32(s.StartTimeRounded300)), get(cg.TSpans, cg.SpRounded300, sp), "span %d", sp)
						require.Equal(t, int64(int32(s.StartTimeRounded3600)), get(cg.TSpans, cg.SpRounded3600, sp))
						require.Equal(t, int64(s.Kind), get(cg.TSpans, cg.SpKind, sp))
						require.Equal(t, int64(s.StatusCode), get(cg.TSpans, cg.SpStatusCode, sp))
						sp++
					}
				}
			}
		}
		require.Equal(t, q.Rows(cg.TSpans), sp)
		require.Equal(t, q.Rows(cg.TServiceStats), ss)
	}
}
