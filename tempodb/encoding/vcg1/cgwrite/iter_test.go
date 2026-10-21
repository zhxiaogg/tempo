package cgwrite_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgwrite"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1/internal/cgtest"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

// TestTraceIteratorRoundTrip writes vParquet5 rows and reads them back with the sequential
// iterator: every rebuilt row deconstructs to the parquet row of the original (what vParquet5
// would store and its compactor compares), for several window sizes and writer limits.
func TestTraceIteratorRoundTrip(t *testing.T) {
	traces := cgtest.Traces(2000, 7)
	dc := cgtest.Dedicated
	sch, _, _ := vparquet5.SchemaWithDynamicChanges(dc)
	conv := vparquet5.NewTraceConverter(dc)
	want := make([]*vparquet5.Trace, len(traces))
	for i, tr := range traces {
		want[i], _ = conv.Convert(tr.ID, tr.Trace, nil)
	}
	for _, tc := range []struct {
		name string
		opts cgwrite.Options
	}{
		{"defaults", cgwrite.Options{}},
		{"tiny", cgwrite.Options{PageBytes: 2 << 10, AttrPageBytes: 8 << 10, ChunkBytes: 16 << 10, KmBytes: 64 << 10, PartBytes: 4 << 10}},
	} {
		tc.opts.Dedicated = dc
		file := writeRows(t, want, tc.opts, 37)
		rd, err := cg.OpenKnown(memRR(file.b), int64(len(file.b)), file.res.FooterLen)
		require.NoError(t, err)
		ctx := context.Background()
		q, err := rd.NewRequest(ctx)
		require.NoError(t, err)
		for _, ws := range []int{1, 100, 0} {
			t.Run(tc.name, func(t *testing.T) {
				it := rd.NewTraceIterator(q.F, dc, ws)
				defer it.Close()
				est := vparquet5.NewSizeEstimator(dc)
				for i, w := range want {
					id, err := it.Peek(ctx)
					require.NoError(t, err)
					require.Equal(t, w.TraceID, id)
					if i%101 == 50 {
						require.NoError(t, it.Skip(ctx))
						continue
					}
					got, err := it.Next(ctx)
					require.NoError(t, err)
					require.NotNil(t, got)
					require.Equal(t, w.TraceIDText, got.TraceIDText)
					wr, gr := sch.Deconstruct(nil, w), sch.Deconstruct(nil, got)
					require.True(t, wr.Equal(gr), "trace %d (%x) differs", i, w.TraceID)
					require.Equal(t, est.ProtoSize(w), est.ProtoSize(got))
				}
				id, err := it.Peek(ctx)
				require.NoError(t, err)
				require.Nil(t, id)
				got, err := it.Next(ctx)
				require.NoError(t, err)
				require.Nil(t, got)
				require.Positive(t, it.Stats().Requests)
			})
		}
	}
}

func TestTraceIteratorCancel(t *testing.T) {
	traces := cgtest.Traces(500, 3)
	conv := vparquet5.NewTraceConverter(cgtest.Dedicated)
	rows := make([]*vparquet5.Trace, len(traces))
	for i, tr := range traces {
		rows[i], _ = conv.Convert(tr.ID, tr.Trace, nil)
	}
	file := writeRows(t, rows, cgwrite.Options{Dedicated: cgtest.Dedicated}, 10)
	rd, err := cg.OpenKnown(memRR(file.b), int64(len(file.b)), file.res.FooterLen)
	require.NoError(t, err)
	q, err := rd.NewRequest(context.Background())
	require.NoError(t, err)
	it := rd.NewTraceIterator(q.F, cgtest.Dedicated, 1)
	ctx, cancel := context.WithCancel(context.Background())
	_, err = it.Next(ctx)
	require.NoError(t, err)
	cancel()
	for err == nil {
		_, err = it.Next(ctx)
	}
	require.ErrorIs(t, err, context.Canceled)
}

type written struct {
	b   []byte
	res cgwrite.Result
}

// writeRows writes rows (cutting an anchor every cutEvery rows) to a file in memory.
func writeRows(t *testing.T, rows []*vparquet5.Trace, opts cgwrite.Options, cutEvery int) written {
	var out memFile
	w, err := cgwrite.New(&out, opts)
	require.NoError(t, err)
	for i, r := range rows {
		if cutEvery > 0 && i > 0 && i%cutEvery == 0 {
			w.CutAnchor()
		}
		require.NoError(t, w.Add(r))
	}
	res, err := w.Close()
	require.NoError(t, err)
	return written{out.b, res}
}

type memFile struct{ b []byte }

func (m *memFile) Write(p []byte) (int, error) { m.b = append(m.b, p...); return len(p), nil }
