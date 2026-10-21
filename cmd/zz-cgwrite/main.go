// zz-cgwrite converts a vParquet5 block to a vCG1 block through vcg1.CreateBlock and verifies
// the result (test tooling for the CG format, not part of Tempo).
//
//	zz-cgwrite -mode convert -src <vp5 block dir> -dst <root dir> [-block <uuid>]
//	    Streams the vP5 rows in order (trace-ID order) as tempopb traces into CreateBlock with
//	    an anchor cut at every source row group, then writes shards.json (source row group ->
//	    anchor range, for StartPage/TotalPages = source row groups). Reports wall time, peak
//	    heap and max RSS.
//	zz-cgwrite -mode verify -src <vp5 block dir> -cg <cg block dir> [-ids sample_ids.json] [-random N]
//	    FindTraceByID on the CG block vs vparquet5 FindTraceByID for the sample ids, and vs the
//	    vP5 row (what vparquet5 FindTraceByID returns) for N random traces.
//	zz-cgwrite -mode stats -file block.cg [-footer-len n]
//	    Footer, chunk, page, run and per-table byte counts of a CG file.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"github.com/parquet-go/parquet-go"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	v1 "github.com/grafana/tempo/v3/pkg/tempopb/common/v1"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/backend/local"
	"github.com/grafana/tempo/v3/tempodb/encoding"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1"
	cg "github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgfile"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1/cgwrite"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

func main() {
	var (
		mode      = flag.String("mode", "", "convert | verify | stats")
		src       = flag.String("src", "", "vParquet5 block directory (<root>/<tenant>/<uuid>)")
		dst       = flag.String("dst", "", "convert: root directory of the output backend")
		blockID   = flag.String("block", "", "convert: output block id (default: new)")
		cgDir     = flag.String("cg", "", "verify: CG block directory")
		ids       = flag.String("ids", "", "verify: sample ids json ([{id}...])")
		random    = flag.Int("random", 0, "verify: random traces to compare")
		seed      = flag.Int64("seed", 1, "verify: random seed")
		file      = flag.String("file", "", "stats: block.cg path")
		viaProto  = flag.Bool("proto", false, "convert: pass proto traces (CreateBlock converts them, re-sorting spans) instead of the vP5 rows")
		zlevel    = flag.Int("zstd", 0, "write-file: klauspost zstd level (1 fastest, 2 default, 3 better)")
		kmBytes   = flag.Int("km-bytes", 0, "write-file: scan-copy buffer budget (0: default)")
		dumpPath  = flag.String("dump", "", "dump: output file; convert: read traces from this dump instead of the parquet file")
		footerLen = flag.Uint("footer-len", 0, "stats: footer length (default: from the trailer)")
	)
	flag.StringVar(&heapProf, "heapprof", "", "convert: write a heap profile here after -heapprof-at traces")
	flag.Int64Var(&heapProfAt, "heapprof-at", 300000, "convert: traces before the heap profile")
	flag.Parse()
	var err error
	switch *mode {
	case "convert":
		err = convert(*src, *dst, *blockID, *dumpPath, *viaProto)
	case "write-file":
		err = writeFile(*src, *dumpPath, *file, *kmBytes, *zlevel)
	case "dump":
		err = dump(*src, *dumpPath)
	case "verify":
		err = verify(*src, *cgDir, *ids, *random, *seed)
	case "stats":
		err = stats(*file, uint32(*footerLen))
	default:
		err = errors.New("-mode convert|verify|stats")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

var (
	heapProf   string
	heapProfAt int64
)

func openBackend(root string) (backend.Reader, backend.Writer, error) {
	rr, ww, _, err := local.New(&local.Config{Path: root})
	if err != nil {
		return nil, nil, err
	}
	return backend.NewReader(rr), backend.NewWriter(ww), nil
}

// blockAt opens the backend holding block dir <root>/<tenant>/<uuid> and reads its meta.
func blockAt(dir string) (backend.Reader, *backend.BlockMeta, error) {
	dir = filepath.Clean(dir)
	r, _, err := openBackend(filepath.Dir(filepath.Dir(dir)))
	if err != nil {
		return nil, nil, err
	}
	id, err := uuid.Parse(filepath.Base(dir))
	if err != nil {
		return nil, nil, err
	}
	m, err := r.BlockMeta(context.Background(), id, filepath.Base(filepath.Dir(dir)))
	return r, m, err
}

// vp5Rows reads the rows of a vParquet5 data.parquet in order.
type vp5Rows struct {
	f       *os.File
	pf      *parquet.File
	r       *parquet.GenericReader[*vparquet5.Trace]
	rgStart []int64 // first row of each row group
	buf     []*vparquet5.Trace
	pos     int
	n       int
	row     int64 // index of the next row
}

func openRows(dir string, dc backend.DedicatedColumns) (*vp5Rows, error) {
	f, err := os.Open(filepath.Join(dir, vparquet5.DataFileName))
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	pf, err := parquet.OpenFile(f, st.Size(), parquet.SkipBloomFilters(true), parquet.SkipPageIndex(true))
	if err != nil {
		return nil, err
	}
	_, _, readerOpts := vparquet5.SchemaWithDynamicChanges(dc)
	v := &vp5Rows{f: f, pf: pf, r: parquet.NewGenericReader[*vparquet5.Trace](pf, readerOpts...), buf: make([]*vparquet5.Trace, 64)}
	var n int64
	for _, rg := range pf.RowGroups() {
		v.rgStart = append(v.rgStart, n)
		n += rg.NumRows()
	}
	return v, nil
}

// next returns the next row (nil at the end) and its index. Rows are fresh objects.
func (v *vp5Rows) next() (*vparquet5.Trace, int64, error) {
	if v.pos == v.n {
		for i := range v.buf {
			v.buf[i] = &vparquet5.Trace{}
		}
		n, err := v.r.Read(v.buf)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, err
		}
		if n == 0 {
			return nil, 0, nil
		}
		v.pos, v.n = 0, n
	}
	t := v.buf[v.pos]
	v.buf[v.pos] = nil
	v.pos++
	v.row++
	return t, v.row - 1, nil
}

func (v *vp5Rows) close() {
	_ = v.r.Close()
	_ = v.f.Close()
}

// rowSource yields the source rows in order: the row and whether it starts a row group.
type rowSource func() (*vparquet5.Trace, bool, error)

// iter is the iterator CreateBlock reads: the vP5 rows as they are (vcg1.RowIterator), with an
// anchor cut at every source row group.
type iter struct {
	next    rowSource
	meta    *backend.BlockMeta
	cut     bool
	rgStart []int64
	count   atomic.Int64
}

func (it *iter) NextRow(context.Context) (*vparquet5.Trace, error) {
	tr, rg, err := it.next()
	if err != nil || tr == nil {
		return nil, err
	}
	n := it.count.Add(1) - 1
	if rg {
		it.rgStart = append(it.rgStart, n)
	}
	it.cut = rg && n > 0
	return tr, nil
}

// Next is the proto form of NextRow (CreateBlock uses NextRow).
func (it *iter) Next(ctx context.Context) (common.ID, *tempopb.Trace, error) {
	tr, err := it.NextRow(ctx)
	if err != nil || tr == nil {
		return nil, nil, err
	}
	return tr.TraceID, vparquet5.ParquetTraceToTempopbTrace(it.meta, tr), nil
}

func (it *iter) CutAnchor() bool { return it.cut }

var (
	_ vcg1.RowIterator  = (*iter)(nil)
	_ vcg1.AnchorCutter = (*iter)(nil)
)

func (it *iter) Close() {}

// protoIter hides NextRow: CreateBlock converts proto traces to rows itself (re-sorting and
// recomputing them as for traces from the WAL).
type protoIter struct{ it *iter }

func (p protoIter) Next(ctx context.Context) (common.ID, *tempopb.Trace, error) {
	return p.it.Next(ctx)
}
func (p protoIter) CutAnchor() bool { return p.it.CutAnchor() }
func (p protoIter) Close()          {}

func parquetSource(rows *vp5Rows) rowSource {
	rgs := map[int64]bool{}
	for _, s := range rows.rgStart {
		rgs[s] = true
	}
	return func() (*vparquet5.Trace, bool, error) {
		tr, i, err := rows.next()
		if err != nil || tr == nil {
			return nil, false, err
		}
		return tr, rgs[i], nil
	}
}

type dumpRec struct {
	RG bool
	T  *vparquet5.Trace
}

// dump writes the vP5 rows as a zstd-compressed gob stream, so a conversion can be measured
// without the parquet reader's memory.
func dump(src, path string) error {
	_, meta, err := blockAt(src)
	if err != nil {
		return err
	}
	rows, err := openRows(src, meta.DedicatedColumns)
	if err != nil {
		return err
	}
	defer rows.close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(zw, 1<<20)
	enc := gob.NewEncoder(bw)
	next := parquetSource(rows)
	n := 0
	for {
		tr, rg, err := next()
		if err != nil {
			return err
		}
		if tr == nil {
			break
		}
		if err := enc.Encode(dumpRec{RG: rg, T: tr}); err != nil {
			return err
		}
		n++
	}
	fmt.Printf("dumped %d traces\n", n)
	if err := bw.Flush(); err != nil {
		return err
	}
	return zw.Close()
}

func dumpSource(path string) (rowSource, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	zr, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		return nil, nil, err
	}
	dec := gob.NewDecoder(bufio.NewReaderSize(zr, 1<<20))
	next := func() (*vparquet5.Trace, bool, error) {
		var r dumpRec
		if err := dec.Decode(&r); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, false, nil
			}
			return nil, false, err
		}
		return r.T, r.RG, nil
	}
	return next, func() { zr.Close(); f.Close() }, nil
}

func convert(src, dst, blockID, dumpPath string, viaProto bool) error {
	if src == "" || dst == "" {
		return errors.New("convert needs -src and -dst")
	}
	ctx := context.Background()
	_, srcMeta, err := blockAt(src)
	if err != nil {
		return err
	}
	it := &iter{meta: srcMeta}
	if dumpPath != "" {
		next, closeFn, err := dumpSource(dumpPath)
		if err != nil {
			return err
		}
		defer closeFn()
		it.next = next
	} else {
		rows, err := openRows(src, srcMeta.DedicatedColumns)
		if err != nil {
			return err
		}
		defer rows.close()
		it.next = parquetSource(rows)
	}
	var in common.Iterator = it
	if viaProto {
		in = protoIter{it}
	}
	r, w, err := openBackend(dst)
	if err != nil {
		return err
	}
	id := uuid.New()
	if blockID != "" {
		if id, err = uuid.Parse(blockID); err != nil {
			return err
		}
	}
	meta := backend.NewBlockMeta(srcMeta.TenantID, id, vcg1.VersionString)
	meta.StartTime, meta.EndTime = srcMeta.StartTime, srcMeta.EndTime
	meta.DedicatedColumns = srcMeta.DedicatedColumns
	meta.TotalObjects = srcMeta.TotalObjects
	meta.ReplicationFactor = srcMeta.ReplicationFactor
	meta.CompactionLevel = srcMeta.CompactionLevel

	var peakHeap atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		var profAt uint64
		tk := time.NewTicker(100 * time.Millisecond)
		defer tk.Stop()
		last := time.Now()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				runtime.ReadMemStats(&ms)
				if heapProf != "" && it.count.Load() >= heapProfAt && ms.HeapInuse > profAt+64<<20 {
					// profile at the peak (rewritten whenever the heap grows by 64 MiB)
					profAt = ms.HeapInuse
					if f, err := os.Create(heapProf); err == nil {
						_ = pprof.WriteHeapProfile(f)
						_ = f.Close()
					}
				}
				if ms.HeapInuse > peakHeap.Load() {
					peakHeap.Store(ms.HeapInuse)
				}
				if time.Since(last) > 30*time.Second {
					last = time.Now()
					fmt.Fprintf(os.Stderr, "  %d traces, heap in use %d MiB\n", it.count.Load(), ms.HeapInuse>>20)
				}
			}
		}
	}()
	start := time.Now()
	cfg := &common.BlockConfig{BloomFP: common.DefaultBloomFP, BloomShardSizeBytes: common.DefaultBloomShardSizeBytes}
	out, err := vcg1.CreateBlock(ctx, cfg, meta, in, r, w)
	close(stop)
	<-done
	if err != nil {
		return err
	}
	wall := time.Since(start)
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)

	// shards.json: source row group -> anchor range
	f, err := readFooter(ctx, r, out)
	if err != nil {
		return err
	}
	anchorOf := map[uint32]int{}
	for i, a := range f.Anchors {
		anchorOf[a[cg.TTraces]] = i
	}
	shards := make([][2]int, len(it.rgStart))
	for i, s := range it.rgStart {
		a, ok := anchorOf[uint32(s)]
		if !ok {
			return fmt.Errorf("no anchor at row group %d (trace %d)", i, s)
		}
		shards[i][0] = a
		if i > 0 {
			shards[i-1][1] = a
		}
	}
	shards[len(shards)-1][1] = len(f.Anchors)
	sj, _ := json.Marshal(shards)
	if err := w.Write(ctx, vcg1.ShardsName, uuid.UUID(out.BlockID), out.TenantID, sj, nil); err != nil {
		return err
	}
	fmt.Printf("block %s/%s/%s\n", dst, out.TenantID, out.BlockID)
	fmt.Printf("traces %d, size %d, footer %d, anchors %d, bloom shards %d\n", out.TotalObjects, out.Size_, out.FooterSize, out.TotalRecords, out.BloomShardCount)
	fmt.Printf("wall %s, peak heap in use %d MiB, max RSS %d MiB, user %.1fs sys %.1fs\n", wall.Round(time.Millisecond), peakHeap.Load()>>20, ru.Maxrss>>20,
		time.Duration(ru.Utime.Nano()).Seconds(), time.Duration(ru.Stime.Nano()).Seconds())
	return nil
}

var zdec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))

func readFooter(ctx context.Context, r backend.Reader, m *backend.BlockMeta) (*cg.Footer, error) {
	b := make([]byte, m.FooterSize)
	if err := r.ReadRange(ctx, cg.ObjectName, uuid.UUID(m.BlockID), m.TenantID, m.Size_-cg.TrailerLen-uint64(m.FooterSize), b, nil); err != nil {
		return nil, err
	}
	return cg.DecodeFooter(zdec, b)
}

// bloomPass answers bloom reads with a filter that passes every id (the source block has no
// bloom objects).
type bloomPass struct {
	backend.Reader
	b []byte
}

func (p *bloomPass) Read(ctx context.Context, name string, id uuid.UUID, tenant string, ci *backend.CacheInfo) ([]byte, error) {
	if strings.HasPrefix(name, "bloom-") {
		return p.b, nil
	}
	return p.Reader.Read(ctx, name, id, tenant, ci)
}

func verify(src, cgDir, idsPath string, random int, seed int64) error {
	ctx := context.Background()
	vr, vm, err := blockAt(src)
	if err != nil {
		return err
	}
	cr, cm, err := blockAt(cgDir)
	if err != nil {
		return err
	}
	bf := bloom.New(1, 1)
	bf.Add([]byte{0})
	var bb bytes.Buffer
	if _, err := bf.WriteTo(&bb); err != nil {
		return err
	}
	vblk, err := encoding.OpenBlock(vm, &bloomPass{Reader: vr, b: bb.Bytes()})
	if err != nil {
		return err
	}
	cblk, err := encoding.OpenBlock(cm, cr)
	if err != nil {
		return err
	}
	if err := cblk.Validate(ctx); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	opts := common.DefaultSearchOptions()
	cgFind := func(id []byte) (*tempopb.Trace, error) {
		resp, err := cblk.FindTraceByID(ctx, id, opts)
		if err != nil {
			return nil, err
		}
		return resp.Trace, nil
	}
	bad := 0
	report := func(what string, id []byte, want, got *tempopb.Trace) {
		if want == nil || got == nil || !bytes.Equal(canon(want), canon(got)) {
			if bad++; bad <= 5 {
				fmt.Printf("MISMATCH %s %x: want nil=%v got nil=%v\n", what, id, want == nil, got == nil)
				if bad == 1 && os.Getenv("CG_DIFF_DIR") != "" {
					wj, _ := json.MarshalIndent(want, "", " ")
					gj, _ := json.MarshalIndent(got, "", " ")
					_ = os.WriteFile(filepath.Join(os.Getenv("CG_DIFF_DIR"), "want.json"), wj, 0o644)
					_ = os.WriteFile(filepath.Join(os.Getenv("CG_DIFF_DIR"), "got.json"), gj, 0o644)
				}
			}
		}
	}
	if idsPath != "" {
		raw, err := os.ReadFile(idsPath)
		if err != nil {
			return err
		}
		var list []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		t0 := time.Now()
		for _, e := range list {
			id, err := hex.DecodeString(e.ID)
			if err != nil {
				return err
			}
			want, err := vblk.FindTraceByID(ctx, id, opts)
			if err != nil {
				return err
			}
			got, err := cgFind(id)
			if err != nil {
				return err
			}
			report("sample", id, want.Trace, got)
		}
		fmt.Printf("sample ids: %d compared with vparquet5 FindTraceByID, %d mismatches (%s)\n", len(list), bad, time.Since(t0).Round(time.Millisecond))
	}
	if random > 0 {
		rows, err := openRows(src, vm.DedicatedColumns)
		if err != nil {
			return err
		}
		defer rows.close()
		total := rows.pf.NumRows()
		rnd := rand.New(rand.NewSource(seed))
		pick := map[int64]bool{}
		for len(pick) < random && int64(len(pick)) < total {
			pick[rnd.Int63n(total)] = true
		}
		before, n := bad, 0
		t0 := time.Now()
		for {
			tr, i, err := rows.next()
			if err != nil {
				return err
			}
			if tr == nil {
				break
			}
			if !pick[i] {
				continue
			}
			got, err := cgFind(tr.TraceID)
			if err != nil {
				return err
			}
			report("random", tr.TraceID, vparquet5.ParquetTraceToTempopbTrace(vm, tr), got)
			n++
		}
		fmt.Printf("random traces: %d of %d compared with the vP5 row, %d mismatches (%s)\n", n, total, bad-before, time.Since(t0).Round(time.Millisecond))
	}
	absent := bytes.Repeat([]byte{0xff}, 16)
	if got, err := cgFind(absent); err != nil || got != nil {
		return fmt.Errorf("absent id: trace %v err %v", got != nil, err)
	}
	if bad > 0 {
		return fmt.Errorf("%d mismatches", bad)
	}
	return nil
}

// canon marshals tr with every attribute list sorted (vParquet5 does not keep attribute order
// across generic and dedicated columns).
func canon(tr *tempopb.Trace) []byte {
	srt := func(a []*v1.KeyValue) {
		sort.SliceStable(a, func(i, j int) bool {
			if a[i].Key != a[j].Key {
				return a[i].Key < a[j].Key
			}
			return a[i].Value.String() < a[j].Value.String()
		})
	}
	for _, rs := range tr.ResourceSpans {
		if rs.Resource != nil {
			srt(rs.Resource.Attributes)
		}
		for _, ss := range rs.ScopeSpans {
			if ss.Scope != nil {
				srt(ss.Scope.Attributes)
			}
			for _, sp := range ss.Spans {
				srt(sp.Attributes)
				for _, e := range sp.Events {
					srt(e.Attributes)
				}
				for _, l := range sp.Links {
					srt(l.Attributes)
				}
			}
		}
	}
	b, err := tr.Marshal()
	if err != nil {
		panic(err)
	}
	return b
}

type fileRR struct{ f *os.File }

func (r fileRR) ReadRange(_ context.Context, off int64, b []byte) error {
	_, err := r.f.ReadAt(b, off)
	return err
}

func stats(path string, footerLen uint32) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	ctx := context.Background()
	rd, err := cg.Open(ctx, fileRR{f}, st.Size())
	if err != nil {
		return err
	}
	if footerLen == 0 {
		footerLen = uint32(rd.FooterLen())
	}
	q, err := rd.NewRequest(ctx)
	if err != nil {
		return err
	}
	ft := q.F
	var chunks, pages, bufs, fallback int
	tableBytes := make([]int64, cg.NumTables)
	fmt.Printf("file %d bytes, footer %d bytes (compressed), %d anchors\n", st.Size(), footerLen, len(ft.Anchors))
	for t := 0; t < cg.NumTables; t++ {
		for gi := range ft.Groups[t] {
			g, err := q.Group(t, gi)
			if err != nil {
				return err
			}
			chunks += len(g.Chunks)
			pages += len(g.Pages)
			for _, c := range g.Chunks {
				tableBytes[t] += c.Len
			}
			for _, p := range g.Pages {
				for k, b := range p.Bufs {
					bufs++
					if b.Encoding() == cg.EncDeltaLength && hasDict(g, k) {
						fallback++
					}
				}
			}
		}
	}
	runs := 0
	var flushes [cg.NumLevels]int
	for l := 0; l < cg.NumLevels; l++ {
		km, err := q.Km(l)
		if err != nil {
			return err
		}
		flushes[l] = len(km)
		for _, k := range km {
			runs += len(k.Runs)
			tableBytes[cg.KmTable(l)] += k.Len
		}
	}
	keys := make([]int, len(ft.Keys))
	for l := range ft.Keys {
		keys[l] = len(ft.Keys[l])
	}
	fmt.Printf("%d group chunks, %d pages, %d buffers (%d dictionary-fallback pages), %d runs, km flushes per level %v, keys per level %v\n",
		chunks, pages, bufs, fallback, runs, flushes, keys)
	for t := 0; t < cg.NumTables; t++ {
		fmt.Printf("  %-20s rows %11d  bytes %13d\n", cg.TableName(t), ft.Rows[t], tableBytes[t])
	}
	return nil
}

// hasDict reports whether column k of the group has a dictionary in any chunk.
func hasDict(g *cg.Group, k int) bool {
	for _, c := range g.Chunks {
		if c.Dicts[k].Present() {
			return true
		}
	}
	return false
}

// writeFile writes the dump's rows with cgwrite directly to a file (size experiments).
func writeFile(src, dumpPath, out string, kmBytes, zlevel int) error {
	_, meta, err := blockAt(src)
	if err != nil {
		return err
	}
	next, closeFn, err := dumpSource(dumpPath)
	if err != nil {
		return err
	}
	defer closeFn()
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	w, err := cgwrite.New(f, cgwrite.Options{Dedicated: vparquet5.FilterDedicatedColumns(meta.DedicatedColumns), KmBytes: kmBytes, Zstd: zstd.EncoderLevel(zlevel)})
	if err != nil {
		return err
	}
	peak, n := 0, 0
	for {
		tr, rg, err := next()
		if err != nil {
			return err
		}
		if tr == nil {
			break
		}
		if rg && n > 0 {
			w.CutAnchor()
		}
		if err := w.Add(tr); err != nil {
			return err
		}
		if n++; n%1000 == 0 {
			peak = max(peak, w.BufferedBytes())
		}
	}
	res, err := w.Close()
	if err != nil {
		return err
	}
	fmt.Printf("size %d footer %d anchors %d chunks %d pages %d runs %d km flushes %v, peak buffered %d MiB\n", res.Size, res.FooterLen, res.Anchors, res.Chunks, res.Pages, res.Runs, res.KmFlushes, peak>>20)
	return nil
}
