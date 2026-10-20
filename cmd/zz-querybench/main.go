// Command zz-querybench is a throwaway measurement harness for the Rust-vs-Go TraceQL
// querier comparison (see ops/bench/rustquerier/FAIRNESS.md). It is NOT for commit.
//
// It exercises Tempo's REAL read path — pkg/parquetquery iterators, the vparquet5 block
// fetch, and the pkg/traceql engine — over a block on local disk.
//
// Modes:
//
//	-mode serve    A real gRPC server for tempopb.Querier, using Tempo's own committed
//	               generated stubs (pkg/tempopb). SearchBlock follows
//	               modules/querier.(*Querier).SearchBlock: it rebuilds the backend.BlockMeta
//	               from the request, maps StartPage/PagesToSearch onto
//	               common.SearchOptions, wraps the block's Fetch/FetchSpans in
//	               traceql.NewSpansetFetcherWrapperBoth, and hands it to
//	               traceql.Engine.ExecuteSearch. The scan runs on the handler goroutine;
//	               there is no worker pool and no cross-request cache of decoded pages.
//	               THIS is the headline number, and the client's wire time is measured
//	               against exactly the same RPC on the Rust side.
//
//	               Alongside it the same listener serves tempopb.Metrics, whose QueryRange
//	               transcribes modules/querier.(*Querier).queryBlock — the code that really
//	               executes a TraceQL metrics range query against ONE backend block. See
//	               metrics.go for why the per-block executor is the querier's even though the
//	               tempopb.Metrics service itself is registered by the live-store.
//
//	-mode client   Tempo's own generated client (tempopb.NewQuerierClient) driving
//	               SearchBlock at -addr. The same client drives the Go and the Rust
//	               server, so client-side cost is identical (FAIRNESS rules 12-15).
//	               With -rpc metrics it drives tempopb.NewMetricsClient/QueryRange instead
//	               and prints, per iteration, wall nanos plus the response's series count,
//	               total samples and canonical digest.
//
//	-mode suite    Round-2 format benchmark client (suite.go): every config of a JSON suite
//	               file (SearchBlock, QueryRange, FindTraceByID) against one server, with
//	               canonical digests and comparison against go-vp5 references.
//
//	-mode limitcheck  In-process check of whether the Limit-truncated SearchBlock answer is
//	               deterministic and equal to the first N traces in block order (suite.go).
//
//	-mode bench    In-process, no wire cost at all, for the engine-time half of rule 15.
//	               Two backends:
//
//	               -backend iterators  parquetquery SyncIterator + JoinIterator, built the
//	                 way vparquet5's createAttributeIterator builds a
//	                 `{span.<key> = "<value>"}` predicate: a JoinIterator at
//	                 DefinitionLevelResourceSpansILSSpanAttrs (4) over
//	                 rs.ss.Spans.Attrs.Key and rs.ss.Spans.Attrs.Value, each carrying a
//	                 ByteInPredicate. Counts joined attribute rows. This is the level with
//	                 the validated anchors (67,551 / 135,870 / 279,657).
//
//	               -backend engine  traceql.Compile + vparquet5 backendBlock.Fetch with the
//	                 compiled pipeline as the second pass, draining the whole row-group
//	                 range with no result limiting. Counts matching spans.
//
//	               -backend metrics  the same queryBlock transcription the Metrics service
//	                 serves, run in process. Counts series and carries the full ground-truth
//	                 table (labels, per-series samples, sums, digest) in its JSON.
//
//	-mode explain  Parse and plan only, printed as JSON.
//
//	-mode attrs    What attribute keys and values the block actually holds, per scope
//	               (-attr-scope resource|event|span|all), over the same -startrg/-rgs
//	               window. Reporting only, not a benchmark: this is how the resource- and
//	               event-scoped ground-truth queries were chosen instead of guessed. See
//	               attrs.go.
//
//	-mode semantics  Spans-per-resource and spans-per-event counts for one key (and
//	               optionally one value), computed by walking raw parquet columns with NO
//	               traceql engine. It is the independent expectation the engine's own answer
//	               for `{resource.k = "v"}` / `{event.k = "v"}` / `by (event.k)` is checked
//	               against. See semantics.go and docker/README.md, "Resource- and
//	               event-scoped attributes".
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/grafana/dskit/user"
	pq "github.com/parquet-go/parquet-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/grafana/tempo/v3/pkg/api"
	"github.com/grafana/tempo/v3/pkg/parquetquery"
	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/backend/local"
	"github.com/grafana/tempo/v3/tempodb/encoding"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
)

// Mirrors of vparquet5's unexported column paths and definition level, used only by the
// low-level `iterators` backend. Both are asserted against the block's schema at startup.
const (
	columnPathSpanAttrKey = "rs.ss.Spans.Attrs.Key"
	columnPathSpanAttrStr = "rs.ss.Spans.Attrs.Value"
	attrDefinitionLevel   = 4 // DefinitionLevelResourceSpansILSSpanAttrs
)

func main() {
	var (
		blockDir = flag.String("block", "", "block directory containing data.parquet and meta.json")
		tenant   = flag.String("tenant", "3", "tenant id; must match the block directory's parent")
		query    = flag.String("query", `{span.node = "2"}`, "TraceQL query")
		rgs      = flag.Int("rgs", 2, "row groups to read (SearchBlockRequest.pagesToSearch)")
		startRG  = flag.Int("startrg", 3, "first row group to read (SearchBlockRequest.startPage)")
		iters    = flag.Int("iters", 5, "timed iterations, after one untimed warm-up")
		latency  = flag.Duration("backend-latency", 0, "serve: mocked latency added before every backend read (e.g. 10ms)")
		mode     = flag.String("mode", "bench", "serve|client|suite|opencost|limitcheck|bench|explain|attrs|semantics")
		backnd   = flag.String("backend", "iterators", "iterators|engine|metrics (-mode bench only)")
		addr     = flag.String("addr", "", "listen address (-mode serve) or dial target (-mode client)")
		limit    = flag.Int("limit", 0, "SearchRequest.Limit = max traces returned. Tempo treats 0 as UNLIMITED on this path (anyCombiner.IsCompleteFor requires limit > 0), and no default is applied inside ExecuteSearch: the familiar 20 comes from the HTTP API parser in the frontend. Keep 0 for the headline measurement so nothing truncates (FAIRNESS rules 19-20).")
		spansPer = flag.Int("spans-per-spanset", 0, "SearchRequest.SpansPerSpanSet. 0 means Tempo's default of 3, which truncates the spans CARRIED BACK per spanset (not the scan). Pass a large value for a non-truncating headline run.")
		rowsFlag = flag.Bool("rows", false, "also collect matched row identifiers (-mode bench -backend iterators)")
		jsonOut  = flag.String("json", "", "also write the result JSON to this path")

		// -mode suite / -mode limitcheck (suite.go)
		suitePath      = flag.String("suite", "", "-mode suite|limitcheck: the JSON suite file (docs/fmtbench/grpc/suite.json)")
		configsCSV     = flag.String("configs", "", "-mode suite: comma-separated config ids to run (default all); -mode limitcheck: the one limited config (default B)")
		refDir         = flag.String("ref", "", "-mode suite: compare every response with the canonical references in this directory")
		writeRef       = flag.String("write-ref", "", "-mode suite: write each query's canonical response here as the reference (merges into index.json)")
		serverName     = flag.String("server-name", "", "-mode suite: label recorded in the result (go-vp5, rs-vp5, ...)")
		failOnMismatch = flag.Bool("fail-on-mismatch", false, "-mode suite: exit 1 if any answer differs from the reference, errors or is nondeterministic")
		waitFor        = flag.Duration("wait", 0, "-mode suite: wait up to this long for -addr to accept connections")
		lcRuns         = flag.Int("runs", 5, "-mode limitcheck: how many times to run each limited query")

		rpc = flag.String("rpc", "search", "which RPC -mode client drives: search (/tempopb.Querier/SearchBlock) or metrics (/tempopb.Metrics/QueryRange)")

		// -mode attrs / -mode semantics (attrs.go, semantics.go)
		attrScope = flag.String("attr-scope", "all", "-mode attrs: which attribute scope to histogram: resource|event|span|all")
		attrTop   = flag.Int("attr-top", 15, "-mode attrs: how many top values to report per key (0 = all)")
		semKey    = flag.String("sem-key", "", "-mode semantics: the attribute key to analyse (required)")
		semValue  = flag.String("sem-value", "", "-mode semantics: the attribute value to analyse (required for the equality checks)")
		semScope  = flag.String("sem-scope", "resource", "-mode semantics: resource|event")

		// TraceQL metrics range-query parameters (-backend metrics / -rpc metrics). These are
		// the QueryRangeRequest fields that have no analogue in SearchBlockRequest.
		step      = flag.Duration("step", 5*time.Minute, "QueryRangeRequest.Step. With start/end derived from the block meta (~25m29s of data) 5m gives a handful of buckets.")
		maxSeries = flag.Int("max-series", 0, "QueryRangeRequest.MaxSeries. 0 means UNLIMITED: metricsEvaluator.Do/DoSpansOnly only break on `maxSeries > 0 && length >= maxSeries`. Keep 0 for the headline measurement so neither side stops early (FAIRNESS rules 19-20).")
		exemplars = flag.Int("exemplars", 0, "QueryRangeRequest.Exemplars. 0 keeps the exemplar machinery out of the measured path.")
		startNs   = flag.Uint64("start-ns", 0, "override QueryRangeRequest.Start (unix nanos). 0 derives it from the block's own meta.json via traceql.TrimToBlockOverlap.")
		endNs     = flag.Uint64("end-ns", 0, "override QueryRangeRequest.End (unix nanos). 0 derives it from the block's own meta.json via traceql.TrimToBlockOverlap.")
	)
	flag.Parse()

	// Tolerate the first draft's flag shape (-mode iterators|engine) rather than silently
	// doing the wrong thing.
	if *mode == "iterators" || *mode == "engine" {
		*backnd, *mode = *mode, "bench"
	}

	mo := metricsOpts{
		Step: *step, MaxSeries: *maxSeries, Exemplars: *exemplars,
		StartNs: *startNs, EndNs: *endNs,
	}

	var (
		out any
		err error
	)
	switch *mode {
	case "serve":
		backendLatency = *latency
		if *blockDir == "" {
			fatal("-block is required")
		}
		if *addr == "" {
			*addr = ":9095"
		}
		err = serve(*blockDir, *tenant, *addr)
	case "client":
		if *addr == "" {
			fatal("-mode client needs -addr host:port")
		}
		switch *rpc {
		case "search":
			out, err = runClient(*addr, *blockDir, *tenant, *query, *startRG, *rgs, *iters, *limit, *spansPer)
		case "metrics":
			out, err = runMetricsClient(*addr, *blockDir, *tenant, *query, *startRG, *rgs, *iters, mo)
		default:
			fatal("unknown -rpc %q, want search|metrics", *rpc)
		}
	case "suite":
		if *addr == "" {
			fatal("-mode suite needs -addr host:port")
		}
		if err := waitTCP(*addr, *waitFor); err != nil {
			fatal("%v", err)
		}
		var sr *suiteResult
		sr, err = runSuite(suiteOpts{addr: *addr, blockDir: *blockDir, tenant: *tenant, suitePath: *suitePath,
			configs: *configsCSV, refDir: *refDir, writeRef: *writeRef, server: *serverName, iters: *iters})
		out = sr
		if err == nil && *failOnMismatch {
			if bad := sr.failures(); len(bad) > 0 {
				emit(out, *jsonOut)
				fatal("%d failing answers, first: %s", len(bad), bad[0])
			}
		}
	case "opencost":
		out, err = runOpenCost(*blockDir, *tenant, *iters)
	case "limitcheck":
		if *blockDir == "" {
			fatal("-block is required")
		}
		c := *configsCSV
		if c == "" {
			c = "B"
		}
		out, err = runLimitCheck(*blockDir, *tenant, *suitePath, c, *lcRuns)
	case "bench":
		if *blockDir == "" {
			fatal("-block is required")
		}
		out, err = runBench(*backnd, *blockDir, *tenant, *query, *startRG, *rgs, *iters, *rowsFlag, mo)
	case "explain":
		if *blockDir == "" {
			fatal("-block is required")
		}
		var q querier
		q, err = newQuerier(*backnd, *blockDir, *tenant, mo)
		if err == nil {
			defer q.Close()
			out, err = q.Explain(*query, *startRG, *rgs)
		}
	case "attrs":
		if *blockDir == "" {
			fatal("-block is required")
		}
		out, err = runAttrs(*blockDir, *startRG, *rgs, *attrTop, *attrScope)
	case "semantics":
		if *blockDir == "" {
			fatal("-block is required")
		}
		if *semKey == "" {
			fatal("-sem-key is required for -mode semantics")
		}
		out, err = runSemantics(*blockDir, *startRG, *rgs, *semScope, *semKey, *semValue)
	default:
		fatal("unknown -mode %q, want serve|client|suite|limitcheck|bench|explain|attrs|semantics", *mode)
	}
	if err != nil {
		fatal("%v", err)
	}
	if out != nil {
		emit(out, *jsonOut)
	}
}

// ---------------------------------------------------------------------------
// environment, recorded on every result so a run can be audited against FAIRNESS.md
// ---------------------------------------------------------------------------

type env struct {
	GoVersion  string `json:"go_version"`
	GOMAXPROCS int    `json:"gomaxprocs"`
	NumCPU     int    `json:"num_cpu"`
	GOGC       string `json:"gogc"`
	GOARCH     string `json:"goarch"`
	GOOS       string `json:"goos"`
}

func currentEnv() env {
	return env{
		GoVersion: runtime.Version(), GOMAXPROCS: runtime.GOMAXPROCS(0),
		NumCPU: runtime.NumCPU(), GOGC: os.Getenv("GOGC"),
		GOARCH: runtime.GOARCH, GOOS: runtime.GOOS,
	}
}

// ---------------------------------------------------------------------------
// mode: serve — tempopb.Querier, SearchBlock following modules/querier
// ---------------------------------------------------------------------------

type querierServer struct {
	// reader over the tenant root, exactly like tempodb's readerWriter holds rw.r
	r             backend.Reader
	defaultTenant string
	engine        *traceql.Engine

	// FindTraceByID only: the block's meta.json, read once at start (what a blocklist poll
	// holds), and the reader with the bloom pass-through (tracebyid.go).
	meta      *backend.BlockMeta
	tbiReader backend.Reader
}

func serve(blockDir, tenant, addr string) error {
	tenantDir := filepath.Dir(blockDir)
	if got := filepath.Base(tenantDir); got != tenant {
		return fmt.Errorf("-tenant %q does not match the block's parent directory %q", tenant, got)
	}
	raw, _, _, err := local.New(&local.Config{Path: filepath.Dir(tenantDir)})
	if err != nil {
		return err
	}
	// Every backend call is counted (iocount.go) so each request's I/O is reported.
	raw = countingRawReader{raw}
	// Fail fast at startup if the block is not readable, rather than on the first RPC.
	rr := backend.NewReader(raw)
	blockID, err := uuid.Parse(filepath.Base(blockDir))
	if err != nil {
		return fmt.Errorf("block dir basename must be the block uuid: %w", err)
	}
	meta, err := rr.BlockMeta(context.Background(), blockID, tenant)
	if err != nil {
		return fmt.Errorf("reading meta.json: %w", err)
	}
	tbi, err := newBloomPassReader(rr)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	gs := grpc.NewServer(grpc.MaxSendMsgSize(256<<20), grpc.MaxRecvMsgSize(64<<20))
	tempopb.RegisterQuerierServer(gs, &querierServer{r: rr, defaultTenant: tenant, engine: traceql.NewEngine(), meta: meta, tbiReader: tbi})
	// Both services on the ONE listener, so a single container serves the search and the
	// metrics comparison under the same CPU budget.
	tempopb.RegisterMetricsServer(gs, &metricsServer{r: rr, defaultTenant: tenant})

	e, _ := json.Marshal(currentEnv())
	fmt.Fprintf(os.Stderr, "zz-querybench: serving tempopb.Querier/SearchBlock, tempopb.Querier/FindTraceByID and tempopb.Metrics/QueryRange on %s tenant=%s block=%s env=%s\n",
		ln.Addr(), tenant, blockID, e)
	return gs.Serve(ln)
}

// SearchBlock mirrors modules/querier.(*Querier).SearchBlock. The only deliberate
// differences, all recorded in docker/README.md:
//
//   - the tenant comes from the x-scope-orgid metadata if present, else from -tenant,
//     instead of from Tempo's auth middleware;
//   - per-tenant overrides (MaxBytesPerTrace, UnsafeQueryHints, SpanPruningAwareness,
//     EngineBytesTracking) are absent, so no CompileOptions are passed — this is the
//     default behaviour a querier with no overrides set would have;
//   - the fetcher calls encoding.OpenBlock(meta, r).Fetch directly, which is verbatim what
//     tempodb.(*readerWriter).Fetch does, minus rw.cfg.Search.ApplyToOptions (we use
//     common.DefaultSearchOptions, the same base the querier starts from);
//   - no tracing spans or Prometheus observations.
//
// The scan runs on this handler goroutine. No decoded pages are retained between requests:
// encoding.OpenBlock is called per Fetch, as in tempodb.
func (s *querierServer) SearchBlock(ctx context.Context, req *tempopb.SearchBlockRequest) (*tempopb.SearchResponse, error) {
	// tempopb.SearchResponse has no timings field, so the server-side engine time is logged
	// to stderr instead of invented into the wire format. FAIRNESS rule 15 asks for the
	// engine time the server reports for itself; docker/run.sh reads these lines back out of
	// `docker logs`.
	rt := startRPC(ctx, "SearchBlock")
	handlerStart := time.Now()
	tenantID, err := user.ExtractOrgID(ctx)
	if err != nil || tenantID == "" {
		tenantID = s.defaultTenant
	}

	blockID, err := backend.ParseUUID(req.BlockID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	dc, err := backend.DedicatedColumnsFromTempopb(req.DedicatedColumns)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	meta := &backend.BlockMeta{
		Version:          req.Version,
		TenantID:         tenantID,
		Size_:            req.Size_,
		IndexPageSize:    req.IndexPageSize,
		TotalRecords:     req.TotalRecords,
		BlockID:          blockID,
		FooterSize:       req.FooterSize,
		DedicatedColumns: dc,
	}

	opts := common.DefaultSearchOptions()
	opts.StartPage = int(req.StartPage)
	opts.TotalPages = int(req.PagesToSearch)

	if !api.IsTraceQLQuery(req.SearchReq) {
		return nil, status.Error(codes.InvalidArgument, "this harness only serves TraceQL queries; set searchReq.Query")
	}

	fetcher := traceql.NewSpansetFetcherWrapperBoth(
		func(ctx context.Context, fq traceql.FetchSpansRequest) (traceql.FetchSpansResponse, error) {
			blk, err := encoding.OpenBlock(meta, s.r)
			if err != nil {
				return traceql.FetchSpansResponse{}, err
			}
			return blk.Fetch(ctx, fq, opts)
		},
		func(ctx context.Context, fq traceql.FetchSpansRequest) (traceql.FetchSpansOnlyResponse, error) {
			blk, err := encoding.OpenBlock(meta, s.r)
			if err != nil {
				return traceql.FetchSpansOnlyResponse{}, err
			}
			return blk.FetchSpans(ctx, fq, opts)
		},
	)

	resp, err := s.engine.ExecuteSearch(ctx, req.SearchReq, fetcher)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	var matched uint64
	var spans int
	for _, tr := range resp.Traces {
		for _, ss := range tr.SpanSets {
			matched += uint64(ss.Matched)
			spans += len(ss.Spans)
		}
	}
	// Measured before proto marshalling, which happens in the gRPC layer after we return:
	// this is engine + metadata assembly, and the client's wall time minus this is the hop.
	fmt.Fprintf(os.Stderr, "searchblock server_ns=%d traces=%d spans=%d matched=%d startPage=%d pagesToSearch=%d limit=%d\n",
		time.Since(handlerStart).Nanoseconds(), len(resp.Traces), spans, matched,
		req.StartPage, req.PagesToSearch, req.SearchReq.GetLimit())
	rt.done(fmt.Sprintf("traces=%d spans=%d matched=%d start_page=%d pages=%d limit=%d inspected_bytes=%d",
		len(resp.Traces), spans, matched, req.StartPage, req.PagesToSearch, req.SearchReq.GetLimit(), resp.GetMetrics().GetInspectedBytes()))

	return resp, nil
}

// The remaining tempopb.Querier methods are out of scope. Returning UNIMPLEMENTED is honest;
// the benchmark calls only SearchBlock and FindTraceByID (tracebyid.go).

func (*querierServer) SearchRecent(context.Context, *tempopb.SearchRequest) (*tempopb.SearchResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in zz-querybench")
}

func (*querierServer) SearchTags(context.Context, *tempopb.SearchTagsRequest) (*tempopb.SearchTagsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in zz-querybench")
}

func (*querierServer) SearchTagsV2(context.Context, *tempopb.SearchTagsRequest) (*tempopb.SearchTagsV2Response, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in zz-querybench")
}

func (*querierServer) SearchTagValues(context.Context, *tempopb.SearchTagValuesRequest) (*tempopb.SearchTagValuesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in zz-querybench")
}

func (*querierServer) SearchTagValuesV2(context.Context, *tempopb.SearchTagValuesRequest) (*tempopb.SearchTagValuesV2Response, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in zz-querybench")
}

// ---------------------------------------------------------------------------
// mode: client — Tempo's own generated client, drives BOTH servers
// ---------------------------------------------------------------------------

type clientIter struct {
	WallNs      int64   `json:"wall_ns"`
	WallMs      float64 `json:"wall_ms"`
	Traces      int     `json:"traces"`
	SpansSetsIn int     `json:"span_sets"`
	Spans       int     `json:"spans"`
	// SpanSet.matched is the engine's own per-spanset count BEFORE SpansPerSpanSet
	// truncation, so this is the match total the two servers must agree on.
	Matched uint64 `json:"matched_total"`

	// True if any spanset came back with fewer spans than its own matched count, i.e.
	// SpansPerSpanSet truncated the response body.
	SpansTruncated bool `json:"spans_truncated"`

	InspectedTraces uint32 `json:"inspected_traces"`
	InspectedBytes  uint64 `json:"inspected_bytes"`
	InspectedSpans  uint64 `json:"inspected_spans"`
	BackendReads    uint64 `json:"backend_reads"`
	BackendBytes    uint64 `json:"backend_bytes"`
}

type clientResult struct {
	Mode  string `json:"mode"`
	Addr  string `json:"addr"`
	Query string `json:"query"`

	StartRowGroup int `json:"start_row_group"`
	RowGroups     int `json:"row_groups"`
	Limit         int `json:"limit"`

	RPC       string `json:"rpc"`
	Semantics string `json:"semantics"`

	Warmup clientIter   `json:"warmup"`
	Iters  []clientIter `json:"iters"`

	// The numbers that must agree between the two servers.
	Traces  int    `json:"traces"`
	Spans   int    `json:"spans"`
	Matched uint64 `json:"matched_total"`

	// Truncation proof, per FAIRNESS rules 19-20. TracesAtLimit true means the trace
	// count hit the cap and the scan stopped early, so the number is not a scan
	// measurement. SpansTruncated true means the response body dropped spans.
	TracesAtLimit  bool `json:"traces_at_limit"`
	SpansTruncated bool `json:"spans_truncated"`

	MedianNs int64   `json:"median_ns"`
	MinNs    int64   `json:"min_ns"`
	MaxNs    int64   `json:"max_ns"`
	MedianMs float64 `json:"median_ms"`
	MinMs    float64 `json:"min_ms"`
	MaxMs    float64 `json:"max_ms"`

	// A sample of the first trace, so a reader can see real metadata came back rather
	// than an empty shell.
	FirstTrace json.RawMessage `json:"first_trace,omitempty"`

	Env env `json:"env"`
}

func runClient(addr, blockDir, tenant, query string, startRG, nRGs, iters, limit, spansPer int) (*clientResult, error) {
	if iters < 1 {
		return nil, errors.New("-iters must be >= 1")
	}

	req, err := buildSearchBlockRequest(blockDir, tenant, query, startRG, nRGs, limit, spansPer)
	if err != nil {
		return nil, err
	}

	cc, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256<<20)))
	if err != nil {
		return nil, err
	}
	defer cc.Close()
	cl := tempopb.NewQuerierClient(cc)

	// Tempo's servers read the tenant from this metadata; the Rust server may ignore it.
	ctx := user.InjectOrgID(context.Background(), tenant)
	ctx, err = user.InjectIntoGRPCRequest(ctx)
	if err != nil {
		return nil, err
	}

	res := &clientResult{
		Mode: "client", Addr: addr, Query: query,
		StartRowGroup: startRG, RowGroups: nRGs, Limit: limit,
		RPC: "/tempopb.Querier/SearchBlock",
		Semantics: "traces returned (capped by SearchRequest.Limit) and spans in their spanSets; " +
			"matched_total sums SpanSet.matched, the engine's pre-truncation per-spanset count, " +
			"and is the figure the two servers must agree on",
		Env: currentEnv(),
	}

	one := func() (clientIter, error) {
		var ci clientIter
		t0 := time.Now()
		resp, err := cl.SearchBlock(ctx, req)
		ci.WallNs = time.Since(t0).Nanoseconds()
		ci.WallMs = float64(ci.WallNs) / 1e6
		if err != nil {
			return ci, err
		}
		ci.Traces = len(resp.Traces)
		for _, tr := range resp.Traces {
			ci.SpansSetsIn += len(tr.SpanSets)
			for _, ss := range tr.SpanSets {
				ci.Spans += len(ss.Spans)
				ci.Matched += uint64(ss.Matched)
				if len(ss.Spans) < int(ss.Matched) {
					ci.SpansTruncated = true
				}
			}
		}
		if m := resp.Metrics; m != nil {
			ci.InspectedTraces, ci.InspectedBytes, ci.InspectedSpans = m.InspectedTraces, m.InspectedBytes, m.InspectedSpans
			ci.BackendReads, ci.BackendBytes = m.BackendReads, m.BackendBytes
		}
		if res.FirstTrace == nil && len(resp.Traces) > 0 {
			if b, err := json.Marshal(resp.Traces[0]); err == nil {
				res.FirstTrace = b
			}
		}
		return ci, nil
	}

	// Untimed warm-up: warms the OS page cache on the server, for both sides equally.
	warm, err := one()
	if err != nil {
		return nil, err
	}
	res.Warmup = warm

	for i := 0; i < iters; i++ {
		ci, err := one()
		if err != nil {
			return nil, err
		}
		if ci.Matched != warm.Matched || ci.Traces != warm.Traces || ci.Spans != warm.Spans {
			return nil, fmt.Errorf("server is nondeterministic: warm-up traces=%d spans=%d matched=%d, iteration %d gave traces=%d spans=%d matched=%d",
				warm.Traces, warm.Spans, warm.Matched, i, ci.Traces, ci.Spans, ci.Matched)
		}
		res.Iters = append(res.Iters, ci)
	}

	res.Traces, res.Spans, res.Matched = warm.Traces, warm.Spans, warm.Matched
	res.TracesAtLimit = limit > 0 && warm.Traces >= limit
	res.SpansTruncated = warm.SpansTruncated
	if res.TracesAtLimit {
		fmt.Fprintf(os.Stderr, "zz-querybench: WARNING traces=%d hit Limit=%d, so the search stopped early and this is NOT a full-scan measurement (FAIRNESS rules 19-20). Use -limit 0 for unlimited.\n", warm.Traces, limit)
	}
	ns := make([]int64, 0, len(res.Iters))
	for _, ci := range res.Iters {
		ns = append(ns, ci.WallNs)
	}
	res.MinNs, res.MedianNs, res.MaxNs = stats(ns)
	res.MinMs, res.MedianMs, res.MaxMs = float64(res.MinNs)/1e6, float64(res.MedianNs)/1e6, float64(res.MaxNs)/1e6
	return res, nil
}

// buildSearchBlockRequest fills the request the way modules/frontend's search sharder does,
// reading the real block meta rather than inventing values.
func buildSearchBlockRequest(blockDir, tenant, query string, startRG, nRGs, limit, spansPer int) (*tempopb.SearchBlockRequest, error) {
	if blockDir == "" {
		return nil, errors.New("-block is required so the client can read meta.json and fill SearchBlockRequest")
	}
	blockID, err := uuid.Parse(filepath.Base(blockDir))
	if err != nil {
		return nil, fmt.Errorf("block dir basename must be the block uuid: %w", err)
	}
	tenantDir := filepath.Dir(blockDir)
	if got := filepath.Base(tenantDir); got != tenant {
		return nil, fmt.Errorf("-tenant %q does not match the block's parent directory %q", tenant, got)
	}
	raw, _, _, err := local.New(&local.Config{Path: filepath.Dir(tenantDir)})
	if err != nil {
		return nil, err
	}
	m, err := backend.NewReader(raw).BlockMeta(context.Background(), blockID, tenant)
	if err != nil {
		return nil, err
	}
	dedCols, err := m.DedicatedColumns.ToTempopb()
	if err != nil {
		return nil, err
	}

	sr := &tempopb.SearchRequest{Query: query, Limit: uint32(limit)}
	if spansPer > 0 {
		sr.SpansPerSpanSet = uint32(spansPer)
	}
	return &tempopb.SearchBlockRequest{
		SearchReq:        sr,
		BlockID:          m.BlockID.String(),
		StartPage:        uint32(startRG),
		PagesToSearch:    uint32(nRGs),
		IndexPageSize:    m.IndexPageSize,
		TotalRecords:     m.TotalRecords,
		Version:          m.Version,
		Size_:            m.Size_,
		FooterSize:       m.FooterSize,
		DedicatedColumns: dedCols,
	}, nil
}

// ---------------------------------------------------------------------------
// mode: bench — in-process, the engine-time half of FAIRNESS rule 15
// ---------------------------------------------------------------------------

type timings struct {
	ParseNs       int64 `json:"parse_ns"`
	PlanNs        int64 `json:"plan_ns"`
	DictResolveNs int64 `json:"dict_resolve_ns"`
	JITCompileNs  int64 `json:"jit_compile_ns"`
	ExecuteNs     int64 `json:"execute_ns"`
	EngineTotalNs int64 `json:"engine_total_ns"`
}

type searchOut struct {
	Count   int64    `json:"match_count"`
	Rows    []uint64 `json:"rows,omitempty"`
	Plan    string   `json:"plan"`
	Timings timings  `json:"timings"`

	InspectedBytes uint64 `json:"inspected_bytes,omitempty"`
	Spansets       int64  `json:"spansets,omitempty"`

	// Set only by the metrics backend.
	GroundTruth *groundTruth `json:"ground_truth,omitempty"`
}

type explainOut struct {
	Plan    string  `json:"plan"`
	Timings timings `json:"timings"`
}

type querier interface {
	Name() string
	Semantics() string
	CountLabel() string
	Search(query string, startRG, nRGs int, withRows bool) (*searchOut, error)
	Explain(query string, startRG, nRGs int) (*explainOut, error)
	Close()
}

func newQuerier(backnd, blockDir, tenant string, mo metricsOpts) (querier, error) {
	switch backnd {
	case "iterators":
		return newIterQuerier(blockDir)
	case "engine":
		return newEngineQuerier(blockDir, tenant)
	case "metrics":
		return newMetricsQuerier(blockDir, tenant, mo)
	default:
		return nil, fmt.Errorf("unknown -backend %q, want iterators|engine|metrics", backnd)
	}
}

type benchResult struct {
	Mode    string `json:"mode"`
	Backend string `json:"backend"`
	Query   string `json:"query"`

	Semantics  string `json:"semantics"`
	CountLabel string `json:"count_label"`

	StartRowGroup    int  `json:"start_row_group"`
	RowGroups        int  `json:"row_groups"`
	RowGroupsHonored bool `json:"row_groups_honored"`

	MatchCount int64 `json:"match_count"`

	Iterations int     `json:"iterations"`
	WarmupNs   int64   `json:"warmup_ns"`
	IterNs     []int64 `json:"iter_ns"`
	MedianNs   int64   `json:"median_ns"`
	MinNs      int64   `json:"min_ns"`
	MaxNs      int64   `json:"max_ns"`
	MedianMs   float64 `json:"median_ms"`
	MinMs      float64 `json:"min_ms"`
	MaxMs      float64 `json:"max_ms"`

	LastTimings timings `json:"last_timings"`
	Plan        string  `json:"plan"`

	InspectedBytes uint64 `json:"inspected_bytes,omitempty"`
	Spansets       int64  `json:"spansets,omitempty"`

	// Set only by -backend metrics: the exact table the Rust side is held to.
	GroundTruth *groundTruth `json:"ground_truth,omitempty"`

	Env env `json:"env"`
}

func runBench(backnd, blockDir, tenant, query string, startRG, nRGs, iters int, withRows bool, mo metricsOpts) (*benchResult, error) {
	if iters < 1 {
		return nil, errors.New("-iters must be >= 1")
	}
	q, err := newQuerier(backnd, blockDir, tenant, mo)
	if err != nil {
		return nil, err
	}
	defer q.Close()

	res := &benchResult{
		Mode: "bench", Backend: q.Name(), Query: query,
		Semantics: q.Semantics(), CountLabel: q.CountLabel(),
		StartRowGroup: startRG, RowGroups: nRGs, RowGroupsHonored: true,
		Iterations: iters, Env: currentEnv(),
	}

	// Untimed warm-up, so the timed iterations measure CPU against a warm OS page cache
	// (FAIRNESS rule 5).
	t0 := time.Now()
	warm, err := q.Search(query, startRG, nRGs, withRows)
	res.WarmupNs = time.Since(t0).Nanoseconds()
	if err != nil {
		return nil, err
	}

	last := warm
	for i := 0; i < iters; i++ {
		t := time.Now()
		out, err := q.Search(query, startRG, nRGs, withRows)
		d := time.Since(t).Nanoseconds()
		if err != nil {
			return nil, err
		}
		if out.Count != warm.Count {
			return nil, fmt.Errorf("nondeterministic count: warm-up %d, iteration %d got %d", warm.Count, i, out.Count)
		}
		// For the metrics backend the count alone is weak: the digest covers every label,
		// timestamp and value, so an iteration that shifted a bucket would be caught here.
		if warm.GroundTruth != nil && out.GroundTruth != nil && out.GroundTruth.Digest != warm.GroundTruth.Digest {
			return nil, fmt.Errorf("nondeterministic results: warm-up digest %s, iteration %d digest %s",
				warm.GroundTruth.Digest, i, out.GroundTruth.Digest)
		}
		res.IterNs = append(res.IterNs, d)
		last = out
	}

	res.MatchCount = warm.Count
	res.LastTimings = last.Timings
	res.Plan = last.Plan
	res.InspectedBytes = last.InspectedBytes
	res.Spansets = last.Spansets
	res.GroundTruth = warm.GroundTruth
	if gt := res.GroundTruth; gt != nil && gt.MaxSeries > 0 && gt.SeriesCount >= gt.MaxSeries {
		fmt.Fprintf(os.Stderr, "zz-querybench: WARNING series=%d reached maxSeries=%d, so metricsEvaluator stopped reading and this is NOT a full-scan measurement (FAIRNESS rules 19-20). Use -max-series 0 for unlimited.\n",
			gt.SeriesCount, gt.MaxSeries)
	}
	res.MinNs, res.MedianNs, res.MaxNs = stats(res.IterNs)
	res.MinMs, res.MedianMs, res.MaxMs = float64(res.MinNs)/1e6, float64(res.MedianNs)/1e6, float64(res.MaxNs)/1e6
	return res, nil
}

// ---------------------------------------------------------------------------
// backend: iterators
// ---------------------------------------------------------------------------

type iterQuerier struct {
	f  *os.File
	pf *pq.File

	keyCol, keyDef int
	valCol, valDef int
}

// newIterQuerier opens the file and parses the parquet footer once. The footer is metadata,
// not data: no decompressed or decoded data pages are retained between queries, so nothing
// the Rust side pays for inside its timed region is skipped here.
func newIterQuerier(blockDir string) (*iterQuerier, error) {
	f, err := os.Open(filepath.Join(blockDir, "data.parquet"))
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	pf, err := pq.OpenFile(f, st.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	q := &iterQuerier{f: f, pf: pf}
	q.keyCol, _, q.keyDef = parquetquery.GetColumnIndexByPath(pf, columnPathSpanAttrKey)
	q.valCol, _, q.valDef = parquetquery.GetColumnIndexByPath(pf, columnPathSpanAttrStr)
	if q.keyCol < 0 || q.valCol < 0 {
		f.Close()
		return nil, fmt.Errorf("columns not found in schema: %s=%d %s=%d",
			columnPathSpanAttrKey, q.keyCol, columnPathSpanAttrStr, q.valCol)
	}
	return q, nil
}

func (q *iterQuerier) Close() { q.f.Close() }

func (q *iterQuerier) Name() string { return "iterators" }

func (q *iterQuerier) Semantics() string {
	return "joined attribute rows: parquetquery.JoinIterator at definition level 4 over " +
		columnPathSpanAttrKey + " x " + columnPathSpanAttrStr + ", ByteInPredicate on each; " +
		"the same construction vparquet5 createAttributeIterator builds for a single " +
		"string-equality span attribute condition. Validated anchors for {span.node = \"2\"}: " +
		"rgs=2 -> 67551, rgs=4 -> 135870, rgs=8 -> 279657."
}

func (q *iterQuerier) CountLabel() string { return "joined attribute results" }

func (q *iterQuerier) rowGroups(startRG, nRGs int) ([]pq.RowGroup, error) {
	all := q.pf.RowGroups()
	if startRG < 0 || nRGs <= 0 || startRG+nRGs > len(all) {
		return nil, fmt.Errorf("block has %d row groups, asked for [%d,%d)", len(all), startRG, startRG+nRGs)
	}
	return all[startRG : startRG+nRGs], nil
}

func (q *iterQuerier) plan(query string, startRG, nRGs int) (func() parquetquery.Iterator, timings, error) {
	var t timings

	t0 := time.Now()
	key, val, err := attrEqualFromQuery(query)
	t.ParseNs = time.Since(t0).Nanoseconds()
	if err != nil {
		return nil, t, err
	}

	t0 = time.Now()
	rowGroups, err := q.rowGroups(startRG, nRGs)
	if err != nil {
		return nil, t, err
	}
	keyPred := parquetquery.NewByteInPredicate([][]byte{[]byte(key)})
	valPred := parquetquery.NewByteInPredicate([][]byte{[]byte(val)})
	build := func() parquetquery.Iterator {
		ki := parquetquery.NewSyncIterator(context.Background(), rowGroups, q.keyCol,
			parquetquery.SyncIteratorOptMaxDefinitionLevel(q.keyDef),
			parquetquery.SyncIteratorOptPredicate(keyPred),
			parquetquery.SyncIteratorOptSelectAs("key"))
		vi := parquetquery.NewSyncIterator(context.Background(), rowGroups, q.valCol,
			parquetquery.SyncIteratorOptMaxDefinitionLevel(q.valDef),
			parquetquery.SyncIteratorOptPredicate(valPred),
			parquetquery.SyncIteratorOptSelectAs("val"))
		return parquetquery.NewJoinIterator(attrDefinitionLevel,
			[]parquetquery.Iterator{ki, vi}, nil)
	}
	t.PlanNs = time.Since(t0).Nanoseconds()
	// Go has no separable dictionary-resolution stage on this path: ByteInPredicate is
	// matched against each chunk's dictionary inside the scan (KeepColumnChunk), not in a
	// stage that can be timed on its own. Reported as 0 and called out in the README
	// rather than guessed at.
	t.DictResolveNs = 0
	t.JITCompileNs = 0 // Go emits no machine code at query time.
	return build, t, nil
}

func (q *iterQuerier) Search(query string, startRG, nRGs int, withRows bool) (*searchOut, error) {
	build, t, err := q.plan(query, startRG, nRGs)
	if err != nil {
		return nil, err
	}

	out := &searchOut{}
	t0 := time.Now()
	it := build()
	out.Plan = it.String()
	var n int64
	for {
		r, err := it.Next()
		if err != nil {
			it.Close()
			return nil, err
		}
		if r == nil {
			break
		}
		n++
		if withRows {
			packed, err := packRowNumber(r.RowNumber)
			if err != nil {
				it.Close()
				return nil, err
			}
			out.Rows = append(out.Rows, packed)
		}
	}
	it.Close()
	t.ExecuteNs = time.Since(t0).Nanoseconds()
	t.EngineTotalNs = t.ParseNs + t.PlanNs + t.DictResolveNs + t.JITCompileNs + t.ExecuteNs
	out.Count = n
	out.Timings = t
	return out, nil
}

func (q *iterQuerier) Explain(query string, startRG, nRGs int) (*explainOut, error) {
	build, t, err := q.plan(query, startRG, nRGs)
	if err != nil {
		return nil, err
	}
	it := build()
	plan := it.String()
	it.Close()
	t.EngineTotalNs = t.ParseNs + t.PlanNs
	return &explainOut{Plan: plan, Timings: t}, nil
}

// packRowNumber packs the hierarchical parquet row number into one uint64. Field widths are
// sized for this block's schema and overflow is an error, never a silent truncation. NOTE:
// this encoding is Go-side; the two implementations' agreement is established by the match
// COUNT (FAIRNESS rule 3), not by comparing these packed values byte for byte.
func packRowNumber(rn parquetquery.RowNumber) (uint64, error) {
	widths := [5]uint{26, 6, 6, 18, 8}
	var packed uint64
	for i, w := range widths {
		v := rn[i]
		if v < 0 {
			v = 0
		}
		if uint64(v) >= uint64(1)<<w {
			return 0, fmt.Errorf("row number level %d = %d overflows %d bits; packing scheme needs widening", i, v, w)
		}
		packed = packed<<w | uint64(v)
	}
	return packed, nil
}

// attrEqualFromQuery runs the query through Tempo's own parser and condition extraction and
// pulls out the single string-equality span attribute condition the iterators backend can
// express. Anything else is an error rather than a silent approximation.
func attrEqualFromQuery(query string) (key, val string, err error) {
	_, _, _, req, err := traceql.Compile(query)
	if err != nil {
		return "", "", fmt.Errorf("traceql.Compile(%q): %w", query, err)
	}
	if len(req.Conditions) != 1 {
		return "", "", fmt.Errorf("-backend iterators needs exactly one extracted condition; %q gave %d: %+v", query, len(req.Conditions), req.Conditions)
	}
	c := req.Conditions[0]
	if c.Op != traceql.OpEqual {
		return "", "", fmt.Errorf("-backend iterators only supports =, got %v", c.Op)
	}
	if c.Attribute.Intrinsic != traceql.IntrinsicNone {
		return "", "", fmt.Errorf("-backend iterators only supports generic span attributes, got intrinsic %v", c.Attribute.Intrinsic)
	}
	if c.Attribute.Scope != traceql.AttributeScopeSpan {
		return "", "", fmt.Errorf("-backend iterators only supports span-scoped attributes (the %s / %s column pair), got scope %v",
			columnPathSpanAttrKey, columnPathSpanAttrStr, c.Attribute.Scope)
	}
	if len(c.Operands) != 1 || c.Operands[0].Type != traceql.TypeString {
		return "", "", fmt.Errorf("-backend iterators only supports a single string operand, got %+v", c.Operands)
	}
	// Same extraction vparquet5's createStringPredicate performs.
	return c.Attribute.Name, c.Operands[0].EncodeToString(false), nil
}

// ---------------------------------------------------------------------------
// backend: engine (in-process, whole range, no result limiting)
// ---------------------------------------------------------------------------

type engineQuerier struct {
	blk common.BackendBlock
}

func newEngineQuerier(blockDir, tenant string) (*engineQuerier, error) {
	blockID, err := uuid.Parse(filepath.Base(blockDir))
	if err != nil {
		return nil, fmt.Errorf("block dir basename must be the block uuid: %w", err)
	}
	tenantDir := filepath.Dir(blockDir)
	if got := filepath.Base(tenantDir); got != tenant {
		return nil, fmt.Errorf("-tenant %q does not match the block's parent directory %q", tenant, got)
	}
	raw, _, _, err := local.New(&local.Config{Path: filepath.Dir(tenantDir)})
	if err != nil {
		return nil, err
	}
	rr := backend.NewReader(raw)
	meta, err := rr.BlockMeta(context.Background(), blockID, tenant)
	if err != nil {
		return nil, err
	}
	blk, err := encoding.OpenBlock(meta, rr)
	if err != nil {
		return nil, err
	}
	return &engineQuerier{blk: blk}, nil
}

func (q *engineQuerier) Close() {}

func (q *engineQuerier) Name() string { return "engine" }

func (q *engineQuerier) Semantics() string {
	return "matching SPANS from traceql.Compile + vparquet5 backendBlock.Fetch with the compiled " +
		"pipeline as the second pass. Row groups are honoured via " +
		"common.SearchOptions{StartPage,TotalPages}, applied by vparquet5 rowGroupsFromFile as " +
		"rgs[StartPage:StartPage+TotalPages]. Result limiting is deliberately NOT applied (no " +
		"MetadataCombiner, no SpansPerSpanSet truncation) so the whole range is read, unlike " +
		"Engine.ExecuteSearch. Counts spans, not attribute rows: a span with two matching " +
		"attributes is one span."
}

func (q *engineQuerier) CountLabel() string { return "matching spans" }

func (q *engineQuerier) Search(query string, startRG, nRGs int, _ bool) (*searchOut, error) {
	var t timings
	ctx := context.Background()

	t0 := time.Now()
	_, _, eval, baseReq, err := traceql.Compile(query)
	t.ParseNs = time.Since(t0).Nanoseconds()
	if err != nil {
		return nil, fmt.Errorf("traceql.Compile(%q): %w", query, err)
	}

	t0 = time.Now()
	opts := common.DefaultSearchOptions()
	opts.StartPage = startRG
	opts.TotalPages = nRGs

	req := *baseReq
	// The same second-pass wiring Engine.ExecuteSearch installs.
	req.SecondPassConditions = append(append([]traceql.Condition(nil), req.SecondPassConditions...),
		traceql.SearchMetaConditionsWithout(req.Conditions, req.AllConditions)...)
	req.SecondPass = func(in *traceql.Spanset) ([]*traceql.Spanset, error) {
		if in == nil || len(in.Spans) == 0 {
			return nil, nil
		}
		return eval([]*traceql.Spanset{in})
	}
	t.PlanNs = time.Since(t0).Nanoseconds()

	t0 = time.Now()
	resp, err := q.blk.Fetch(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	var spans, spansets int64
	for {
		ss, err := resp.Results.Next(ctx)
		if err != nil {
			resp.Results.Close()
			return nil, err
		}
		if ss == nil {
			break
		}
		spansets++
		spans += int64(len(ss.Spans))
		ss.Release()
	}
	resp.Results.Close()
	t.ExecuteNs = time.Since(t0).Nanoseconds()
	t.EngineTotalNs = t.ParseNs + t.PlanNs + t.ExecuteNs

	out := &searchOut{
		Count: spans, Spansets: spansets, Timings: t,
		Plan: fmt.Sprintf("vparquet5.backendBlock.Fetch conditions=%+v secondPass=%+v", req.Conditions, req.SecondPassConditions),
	}
	if resp.Stats != nil {
		out.InspectedBytes = resp.Stats().Bytes
	}
	return out, nil
}

func (q *engineQuerier) Explain(query string, _, _ int) (*explainOut, error) {
	var t timings
	t0 := time.Now()
	expr, reqs, err := traceql.CompileFetchSpanRequests(query)
	t.ParseNs = time.Since(t0).Nanoseconds()
	if err != nil {
		return nil, err
	}
	t.EngineTotalNs = t.ParseNs
	return &explainOut{Plan: fmt.Sprintf("%s\n%+v", expr.String(), reqs), Timings: t}, nil
}

// ---------------------------------------------------------------------------

func stats(ns []int64) (minv, med, maxv int64) {
	if len(ns) == 0 {
		return 0, 0, 0
	}
	s := append([]int64(nil), ns...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	minv, maxv = s[0], s[len(s)-1]
	if len(s)%2 == 1 {
		med = s[len(s)/2]
	} else {
		med = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return minv, med, maxv
}

func emit(v any, path string) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fatal("%v", err)
	}
	fmt.Println(string(b))
	if path != "" {
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			fatal("%v", err)
		}
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "zz-querybench: "+format+"\n", args...)
	os.Exit(1)
}
