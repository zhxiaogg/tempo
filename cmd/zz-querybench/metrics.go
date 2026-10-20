// TraceQL metrics range queries (`| rate()` and friends) for zz-querybench.
//
// # WHERE THE REAL CODE LIVES
//
// The gRPC service `tempopb.Metrics` is registered in exactly one place in Tempo —
// cmd/tempo/app/modules.go:722, `tempopb.RegisterMetricsServer(t.Server.GRPC(), t.liveStore)`
// — so on the wire the Metrics service is the LIVE-STORE's, and it answers from WAL blocks,
// not from a backend parquet block addressed by blockID/startPage/pagesToSearch.
//
// The code that genuinely executes a metrics range query against ONE backend block is
// `modules/querier.(*Querier).queryBlock` (modules/querier/querier_query_range.go). A
// query-frontend's queryRangeSharder (modules/frontend/metrics_query_range_sharder.go,
// buildBackendRequests) fans a user query out into one QueryRangeRequest per block/page range
// and sends each one to a querier over HTTP (api.BuildQueryRangeRequest ->
// Querier.QueryRangeHandler -> Querier.QueryRange -> Querier.queryBlock, because QueryMode is
// not "recent"). So the per-block executor is on the QUERIER; only its transport is HTTP.
//
// This file therefore serves `tempopb.Metrics/QueryRange` — the RPC the benchmark compares
// over the wire, carrying exactly the per-block fields the sharder fills in — with a handler
// that is a line-for-line transcription of `queryBlock`. Deliberate differences, all of them
// the "no overrides configured" default:
//
//   - tenant from x-scope-orgid metadata if present, else -tenant (no auth middleware);
//   - q.limits.* per-tenant overrides are absent: UnsafeQueryHints=false,
//     MetricsSpanOnlyFetch=nil (so metricsEvaluator.spanOnlyFetch stays at its default true
//     and the scan runs through DoSpansOnly), SpanPruningAwareness/EngineBytesTracking unset;
//   - WithTimeOverlapCutoff is passed the querier's own default, 0.2
//     (modules/querier/config.go:70), since there is no YAML to read;
//   - q.store.Fetch/FetchSpans is inlined as encoding.OpenBlock(meta, r).Fetch/FetchSpans,
//     which is verbatim tempodb.(*readerWriter).Fetch minus rw.cfg.Search.ApplyToOptions;
//   - no tracing spans, no observeBackendProcessing.
//
// The block is re-opened per fetch, as tempodb does, so no decoded page survives a request
// (FAIRNESS rule 15b).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/grafana/dskit/user"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	commonv1 "github.com/grafana/tempo/v3/pkg/tempopb/common/v1"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/backend/local"
	"github.com/grafana/tempo/v3/tempodb/encoding"
	"github.com/grafana/tempo/v3/tempodb/encoding/common"
)

// querierDefaultTimeOverlapCutoff is modules/querier/config.go:70 — the value a querier with
// no time_overlap_cutoff in its YAML passes to WithTimeOverlapCutoff.
const querierDefaultTimeOverlapCutoff = 0.2

// metricsOpts carries the QueryRangeRequest fields that have no analogue in SearchBlock.
type metricsOpts struct {
	Step      time.Duration
	MaxSeries int
	Exemplars int
	// StartNs/EndNs override the window derived from the block meta. 0 means derive.
	StartNs uint64
	EndNs   uint64
}

// ---------------------------------------------------------------------------
// the request, built the way modules/frontend's queryRangeSharder builds it
// ---------------------------------------------------------------------------

// buildQueryRangeRequest fills a per-block QueryRangeRequest exactly as
// queryRangeSharder.buildBackendRequests does, and derives the time window from the block's
// OWN meta.json using Tempo's own traceql.TrimToBlockOverlap: a user query wider than the
// block (Start=0, End=+inf) trimmed to the block, then aligned to the step. That is what a
// "query the whole block" request looks like coming out of a real frontend, and it leaves
// start%step == end%step == 0, which is the branch CompileMetricsQueryRange takes to read the
// span start-time column at step precision rather than full nanoseconds.
func buildQueryRangeRequest(blockDir, tenant, query string, startRG, nRGs int, mo metricsOpts) (*tempopb.QueryRangeRequest, error) {
	if blockDir == "" {
		return nil, errors.New("-block is required so the client can read meta.json and fill QueryRangeRequest")
	}
	if mo.Step <= 0 {
		return nil, errors.New("-step must be > 0")
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

	step := uint64(mo.Step.Nanoseconds())

	// The whole-block window, derived by Tempo's own trimming/alignment code.
	wide := &tempopb.QueryRangeRequest{Query: query, Start: 0, End: math.MaxInt64, Step: step}
	start, end, step := traceql.TrimToBlockOverlap(wide, m.StartTime, m.EndTime)

	if mo.StartNs != 0 {
		start = mo.StartNs
	}
	if mo.EndNs != 0 {
		end = mo.EndNs
	}
	if end <= start {
		return nil, fmt.Errorf("derived end %d <= start %d", end, start)
	}

	return &tempopb.QueryRangeRequest{
		Query: query,
		Start: start,
		End:   end,
		Step:  step,
		// QueryMode is left empty. Querier.QueryRange only short-circuits to the live-store
		// ring when QueryMode == "recent", so empty takes the queryBlock path.
		BlockID:          m.BlockID.String(),
		StartPage:        uint32(startRG),
		PagesToSearch:    uint32(nRGs),
		Version:          m.Version,
		Size_:            m.Size_,
		FooterSize:       m.FooterSize,
		DedicatedColumns: dedCols,
		Exemplars:        uint32(mo.Exemplars),
		MaxSeries:        uint32(mo.MaxSeries),
	}, nil
}

// ---------------------------------------------------------------------------
// mode: serve — tempopb.Metrics, QueryRange transcribing Querier.queryBlock
// ---------------------------------------------------------------------------

type metricsServer struct {
	r             backend.Reader
	defaultTenant string
}

var _ tempopb.MetricsServer = (*metricsServer)(nil)

func (s *metricsServer) QueryRange(ctx context.Context, req *tempopb.QueryRangeRequest) (*tempopb.QueryRangeResponse, error) {
	rt := startRPC(ctx, "QueryRange")
	handlerStart := time.Now()

	tenantID, err := user.ExtractOrgID(ctx)
	if err != nil || tenantID == "" {
		tenantID = s.defaultTenant
	}

	resp, err := queryBlockMetrics(ctx, s.r, tenantID, req)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	// tempopb.QueryRangeResponse has no timings field, so the server's own engine time goes to
	// stderr rather than being invented into the wire format (same convention as SearchBlock).
	// docker/run.sh reads these back out of `docker logs`.
	var samples int
	for _, ts := range resp.Series {
		samples += len(ts.Samples)
	}
	fmt.Fprintf(os.Stderr, "queryrange server_ns=%d series=%d samples=%d status=%s start=%d end=%d step=%d maxSeries=%d startPage=%d pagesToSearch=%d\n",
		time.Since(handlerStart).Nanoseconds(), len(resp.Series), samples, resp.Status.String(),
		req.Start, req.End, req.Step, req.MaxSeries, req.StartPage, req.PagesToSearch)
	rt.done(fmt.Sprintf("series=%d samples=%d status=%s start_page=%d pages=%d step=%d inspected_bytes=%d",
		len(resp.Series), samples, resp.Status.String(), req.StartPage, req.PagesToSearch, req.Step, resp.GetMetrics().GetInspectedBytes()))

	return resp, nil
}

// queryBlockMetrics is modules/querier.(*Querier).queryBlock, transcribed. See the file
// comment for the list of deliberate differences.
func queryBlockMetrics(ctx context.Context, r backend.Reader, tenantID string, req *tempopb.QueryRangeRequest) (*tempopb.QueryRangeResponse, error) {
	blockID, err := backend.ParseUUID(req.BlockID)
	if err != nil {
		return nil, err
	}

	dc, err := backend.DedicatedColumnsFromTempopb(req.DedicatedColumns)
	if err != nil {
		return nil, err
	}

	// queryBlock builds the meta from the REQUEST's start/end, not from meta.json. Those are
	// also what it hands to eval.Do as fetcherStart/fetcherEnd, so the request window alone
	// decides the trace-level timestamp filtering decision.
	meta := &backend.BlockMeta{
		Version:          req.Version,
		TenantID:         tenantID,
		StartTime:        time.Unix(0, int64(req.Start)),
		EndTime:          time.Unix(0, int64(req.End)),
		BlockID:          blockID,
		Size_:            req.Size_,
		FooterSize:       req.FooterSize,
		DedicatedColumns: dc,
	}

	opts := common.DefaultSearchOptions()
	opts.StartPage = int(req.StartPage)
	opts.TotalPages = int(req.PagesToSearch)

	// Parse without optimizations to read hints; optimizations are applied by CompileMetricsQueryRange.
	expr, err := traceql.ParseNoOptimizations(req.Query)
	if err != nil {
		return nil, err
	}

	var compileOpts []traceql.CompileOption

	// q.limits.UnsafeQueryHints(tenantID) with no overrides configured is false, so no
	// WithUnsafeHints, and GetFloat below is asked for a safe-hints-only lookup.
	const unsafe = false
	for _, name := range req.SkipASTTransformations {
		compileOpts = append(compileOpts, traceql.WithSkipOptimization(name))
	}
	if v, ok := expr.Hints.GetFloat(traceql.HintTimeOverlapCutoff, unsafe); ok && v >= 0 && v <= 1.0 {
		compileOpts = append(compileOpts, traceql.WithTimeOverlapCutoff(v))
	} else {
		compileOpts = append(compileOpts, traceql.WithTimeOverlapCutoff(querierDefaultTimeOverlapCutoff))
	}
	// MetricsSpanOnlyFetch / SpanPruningAwareness / EngineBytesTracking overrides are nil here.

	eval, err := traceql.NewEngine().CompileMetricsQueryRange(req, compileOpts...)
	if err != nil {
		return nil, err
	}

	f := traceql.NewSpansetFetcherWrapperBoth(
		func(ctx context.Context, fq traceql.FetchSpansRequest) (traceql.FetchSpansResponse, error) {
			blk, err := encoding.OpenBlock(meta, r)
			if err != nil {
				return traceql.FetchSpansResponse{}, err
			}
			return blk.Fetch(ctx, fq, opts)
		},
		func(ctx context.Context, fq traceql.FetchSpansRequest) (traceql.FetchSpansOnlyResponse, error) {
			blk, err := encoding.OpenBlock(meta, r)
			if err != nil {
				return traceql.FetchSpansOnlyResponse{}, err
			}
			return blk.FetchSpans(ctx, fq, opts)
		},
	)

	err = eval.Do(ctx, f, uint64(meta.StartTime.UnixNano()), uint64(meta.EndTime.UnixNano()), int(req.MaxSeries))
	if err != nil {
		return nil, err
	}

	res := eval.Results()
	em := eval.Metrics()

	if req.MaxSeries > 0 && len(res) > int(req.MaxSeries) {
		limitedRes := make(traceql.SeriesSet)
		count := 0
		for k, v := range res {
			if count >= int(req.MaxSeries) {
				break
			}
			limitedRes[k] = v
			count++
		}
		res = limitedRes
	}

	response := &tempopb.QueryRangeResponse{
		Series: res.ToProto(req),
		Metrics: &tempopb.SearchMetrics{
			InspectedBytes:    em.Bytes,
			InspectedSpans:    em.SpansTotal,
			BackendReads:      em.BackendReads,
			BackendBytes:      em.BackendBytes,
			AdditionalMetrics: em.AdditionalMetrics,
		},
	}

	if req.MaxSeries > 0 && len(res) > int(req.MaxSeries) {
		response.Status = tempopb.PartialStatus_PARTIAL
	}

	return response, nil
}

// ---------------------------------------------------------------------------
// ground truth + digest, computed from the response proto so the in-process bench, the gRPC
// client and the Rust server all hash exactly the same thing
// ---------------------------------------------------------------------------

type sampleTruth struct {
	TimestampMs int64   `json:"t_ms"`
	Timestamp   string  `json:"t"`
	Value       float64 `json:"v"`
}

type seriesTruth struct {
	// Labels is the canonical rendering used by the digest: "k=v" pairs sorted by key,
	// joined with ",". An empty label set renders as "" (the ungrouped rate() series).
	Labels  string        `json:"labels"`
	Samples int           `json:"samples"`
	Sum     float64       `json:"sum"`
	Values  []sampleTruth `json:"values"`
}

type groundTruth struct {
	Query string `json:"query"`

	StartNs uint64 `json:"start_ns"`
	EndNs   uint64 `json:"end_ns"`
	StepNs  uint64 `json:"step_ns"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Step    string `json:"step"`
	// IntervalCount is traceql.NewIntervalMapperFromReq(req).IntervalCount(): the number of
	// buckets ToProto will emit per series at most. NaN buckets are dropped, so a series may
	// carry fewer samples than this.
	IntervalCount int `json:"interval_count"`

	StartPage     int `json:"start_page"`
	PagesToSearch int `json:"pages_to_search"`
	MaxSeries     int `json:"max_series"`
	Exemplars     int `json:"exemplars"`

	Status string `json:"status"`

	SeriesCount  int     `json:"series_count"`
	TotalSamples int     `json:"total_samples"`
	SumAllValues float64 `json:"sum_all_values"`
	Digest       string  `json:"digest"`

	Series []seriesTruth `json:"series"`

	InspectedBytes uint64 `json:"inspected_bytes"`
	InspectedSpans uint64 `json:"inspected_spans"`
	BackendReads   uint64 `json:"backend_reads"`
	BackendBytes   uint64 `json:"backend_bytes"`
}

// formatFloat is the shortest representation that round-trips back to the same float64.
// Go's strconv.FormatFloat(v,'g',-1,64) and Rust's `{}` for f64 both produce it, so the two
// implementations can hash the same bytes without agreeing on a format string.
func formatFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func anyValueString(v *commonv1.AnyValue) string {
	if v == nil {
		return ""
	}
	switch x := v.Value.(type) {
	case *commonv1.AnyValue_StringValue:
		return x.StringValue
	case *commonv1.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonv1.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonv1.AnyValue_DoubleValue:
		return formatFloat(x.DoubleValue)
	case *commonv1.AnyValue_BytesValue:
		return hex.EncodeToString(x.BytesValue)
	default:
		return v.String()
	}
}

// summarize turns the response into the ground-truth table and the canonical digest.
//
// Canonical form, one line per series:
//
//	<k1>=<v1>,<k2>=<v2>|t=<tsMs>:v=<val>,t=<tsMs>:v=<val>,...
//
// with label pairs sorted by (key, value), samples in the response's own time order (ToProto
// emits them oldest-first), values as shortest-round-trip floats. Lines are then sorted and
// joined with "\n"; the digest is the hex SHA-256 of that. Nothing about series iteration
// order or map ordering can leak into it.
func summarize(resp *tempopb.QueryRangeResponse) (series []seriesTruth, totalSamples int, sumAll float64, digest string) {
	lines := make([]string, 0, len(resp.GetSeries()))
	series = make([]seriesTruth, 0, len(resp.GetSeries()))

	for _, ts := range resp.GetSeries() {
		pairs := make([]string, 0, len(ts.Labels))
		for _, l := range ts.Labels {
			pairs = append(pairs, l.Key+"="+anyValueString(l.Value))
		}
		sort.Strings(pairs)
		labels := strings.Join(pairs, ",")

		st := seriesTruth{Labels: labels, Samples: len(ts.Samples), Values: make([]sampleTruth, 0, len(ts.Samples))}
		var b strings.Builder
		b.WriteString(labels)
		b.WriteByte('|')
		for i, s := range ts.Samples {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString("t=")
			b.WriteString(strconv.FormatInt(s.TimestampMs, 10))
			b.WriteString(":v=")
			b.WriteString(formatFloat(s.Value))

			st.Sum += s.Value
			st.Values = append(st.Values, sampleTruth{
				TimestampMs: s.TimestampMs,
				Timestamp:   time.UnixMilli(s.TimestampMs).UTC().Format(time.RFC3339Nano),
				Value:       s.Value,
			})
		}
		totalSamples += len(ts.Samples)
		series = append(series, st)
		lines = append(lines, b.String())
	}

	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	digest = hex.EncodeToString(sum[:])

	sort.Slice(series, func(i, j int) bool { return series[i].Labels < series[j].Labels })

	// Summed AFTER sorting. Floating-point addition is not associative, so summing in the
	// response's own (map-iteration) order gives a total that wobbles in the last few bits
	// between runs — 5337.54000000009 vs 5337.540000000077 on the 2,211-series query. Summing
	// the sorted series makes the reported total reproducible, which rule 16 requires.
	for _, st := range series {
		sumAll += st.Sum
	}
	return series, totalSamples, sumAll, digest
}

func newGroundTruth(req *tempopb.QueryRangeRequest, resp *tempopb.QueryRangeResponse) *groundTruth {
	s, total, sumAll, digest := summarize(resp)
	gt := &groundTruth{
		Query:         req.Query,
		StartNs:       req.Start,
		EndNs:         req.End,
		StepNs:        req.Step,
		Start:         time.Unix(0, int64(req.Start)).UTC().Format(time.RFC3339Nano),
		End:           time.Unix(0, int64(req.End)).UTC().Format(time.RFC3339Nano),
		Step:          time.Duration(req.Step).String(),
		IntervalCount: traceql.NewIntervalMapperFromReq(req).IntervalCount(),
		StartPage:     int(req.StartPage),
		PagesToSearch: int(req.PagesToSearch),
		MaxSeries:     int(req.MaxSeries),
		Exemplars:     int(req.Exemplars),
		Status:        resp.Status.String(),
		SeriesCount:   len(resp.GetSeries()),
		TotalSamples:  total,
		SumAllValues:  sumAll,
		Digest:        digest,
		Series:        s,
	}
	if m := resp.Metrics; m != nil {
		gt.InspectedBytes, gt.InspectedSpans = m.InspectedBytes, m.InspectedSpans
		gt.BackendReads, gt.BackendBytes = m.BackendReads, m.BackendBytes
	}
	return gt
}

// ---------------------------------------------------------------------------
// mode: client -rpc metrics — Tempo's own generated MetricsClient, drives BOTH servers
// ---------------------------------------------------------------------------

type metricsIter struct {
	WallNs int64   `json:"wall_ns"`
	WallMs float64 `json:"wall_ms"`

	Series       int     `json:"series"`
	TotalSamples int     `json:"total_samples"`
	SumAllValues float64 `json:"sum_all_values"`
	Digest       string  `json:"digest"`
	Status       string  `json:"status"`

	InspectedBytes uint64 `json:"inspected_bytes"`
	InspectedSpans uint64 `json:"inspected_spans"`
	BackendReads   uint64 `json:"backend_reads"`
	BackendBytes   uint64 `json:"backend_bytes"`
}

type metricsClientResult struct {
	Mode  string `json:"mode"`
	RPC   string `json:"rpc"`
	Addr  string `json:"addr"`
	Query string `json:"query"`

	StartRowGroup int `json:"start_row_group"`
	RowGroups     int `json:"row_groups"`

	Semantics string `json:"semantics"`

	Warmup metricsIter   `json:"warmup"`
	Iters  []metricsIter `json:"iters"`

	// The numbers the two servers must agree on.
	Series       int     `json:"series"`
	TotalSamples int     `json:"total_samples"`
	SumAllValues float64 `json:"sum_all_values"`
	Digest       string  `json:"digest"`
	Status       string  `json:"status"`

	// Truncation proof, FAIRNESS 19-20. True means the evaluator bailed at maxSeries and
	// this is NOT a full-scan measurement.
	MaxSeriesReached bool `json:"max_series_reached"`

	MedianNs int64   `json:"median_ns"`
	MinNs    int64   `json:"min_ns"`
	MaxNs    int64   `json:"max_ns"`
	MedianMs float64 `json:"median_ms"`
	MinMs    float64 `json:"min_ms"`
	MaxMs    float64 `json:"max_ms"`

	GroundTruth *groundTruth `json:"ground_truth"`

	Env env `json:"env"`
}

func runMetricsClient(addr, blockDir, tenant, query string, startRG, nRGs, iters int, mo metricsOpts) (*metricsClientResult, error) {
	if iters < 1 {
		return nil, errors.New("-iters must be >= 1")
	}

	req, err := buildQueryRangeRequest(blockDir, tenant, query, startRG, nRGs, mo)
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
	cl := tempopb.NewMetricsClient(cc)

	ctx := user.InjectOrgID(context.Background(), tenant)
	ctx, err = user.InjectIntoGRPCRequest(ctx)
	if err != nil {
		return nil, err
	}

	res := &metricsClientResult{
		Mode: "client", RPC: "/tempopb.Metrics/QueryRange", Addr: addr, Query: query,
		StartRowGroup: startRG, RowGroups: nRGs,
		Semantics: "TimeSeries returned by traceql.SeriesSet.ToProto; total_samples sums samples " +
			"across series (NaN buckets are dropped by ToProto, so a series can carry fewer than " +
			"interval_count); digest is the hex SHA-256 of sorted canonical " +
			`"k=v,...|t=<ms>:v=<val>,..." lines and is what the two implementations compare`,
		Env: currentEnv(),
	}

	var first *tempopb.QueryRangeResponse

	one := func() (metricsIter, error) {
		var mi metricsIter
		t0 := time.Now()
		resp, err := cl.QueryRange(ctx, req)
		mi.WallNs = time.Since(t0).Nanoseconds()
		mi.WallMs = float64(mi.WallNs) / 1e6
		if err != nil {
			return mi, err
		}
		s, total, sumAll, digest := summarize(resp)
		mi.Series, mi.TotalSamples, mi.SumAllValues, mi.Digest = len(s), total, sumAll, digest
		mi.Status = resp.Status.String()
		if m := resp.Metrics; m != nil {
			mi.InspectedBytes, mi.InspectedSpans = m.InspectedBytes, m.InspectedSpans
			mi.BackendReads, mi.BackendBytes = m.BackendReads, m.BackendBytes
		}
		if first == nil {
			first = resp
		}
		fmt.Fprintf(os.Stderr, "queryrange client wall_ns=%d series=%d samples=%d sum=%s status=%s digest=%s\n",
			mi.WallNs, mi.Series, mi.TotalSamples, formatFloat(mi.SumAllValues), mi.Status, mi.Digest)
		return mi, nil
	}

	// Untimed warm-up: warms the OS page cache on the server, for both sides equally.
	warm, err := one()
	if err != nil {
		return nil, err
	}
	res.Warmup = warm

	for i := 0; i < iters; i++ {
		mi, err := one()
		if err != nil {
			return nil, err
		}
		if mi.Digest != warm.Digest {
			return nil, fmt.Errorf("server is nondeterministic: warm-up digest %s, iteration %d digest %s (series %d vs %d)",
				warm.Digest, i, mi.Digest, warm.Series, mi.Series)
		}
		res.Iters = append(res.Iters, mi)
	}

	res.Series, res.TotalSamples, res.SumAllValues = warm.Series, warm.TotalSamples, warm.SumAllValues
	res.Digest, res.Status = warm.Digest, warm.Status
	res.GroundTruth = newGroundTruth(req, first)

	res.MaxSeriesReached = mo.MaxSeries > 0 && warm.Series >= mo.MaxSeries
	if res.MaxSeriesReached {
		fmt.Fprintf(os.Stderr, "zz-querybench: WARNING series=%d reached maxSeries=%d, so metricsEvaluator.Do stopped reading and this is NOT a full-scan measurement (FAIRNESS rules 19-20). Use -max-series 0 for unlimited.\n",
			warm.Series, mo.MaxSeries)
	}

	ns := make([]int64, 0, len(res.Iters))
	for _, mi := range res.Iters {
		ns = append(ns, mi.WallNs)
	}
	res.MinNs, res.MedianNs, res.MaxNs = stats(ns)
	res.MinMs, res.MedianMs, res.MaxMs = float64(res.MinNs)/1e6, float64(res.MedianNs)/1e6, float64(res.MaxNs)/1e6
	return res, nil
}

// ---------------------------------------------------------------------------
// backend: metrics — in-process, the engine-time half of FAIRNESS rule 15
// ---------------------------------------------------------------------------

type metricsQuerier struct {
	r        backend.Reader
	tenant   string
	blockDir string
	mo       metricsOpts
}

func newMetricsQuerier(blockDir, tenant string, mo metricsOpts) (*metricsQuerier, error) {
	tenantDir := filepath.Dir(blockDir)
	if got := filepath.Base(tenantDir); got != tenant {
		return nil, fmt.Errorf("-tenant %q does not match the block's parent directory %q", tenant, got)
	}
	raw, _, _, err := local.New(&local.Config{Path: filepath.Dir(tenantDir)})
	if err != nil {
		return nil, err
	}
	return &metricsQuerier{r: backend.NewReader(raw), tenant: tenant, blockDir: blockDir, mo: mo}, nil
}

func (q *metricsQuerier) Close() {}

func (q *metricsQuerier) Name() string { return "metrics" }

func (q *metricsQuerier) Semantics() string {
	return "TraceQL metrics range query over ONE block, via the transcription of " +
		"modules/querier.(*Querier).queryBlock in cmd/zz-querybench/metrics.go: " +
		"traceql.ParseNoOptimizations for hints, Engine.CompileMetricsQueryRange with the " +
		"querier's default WithTimeOverlapCutoff(0.2) and no per-tenant overrides, " +
		"NewSpansetFetcherWrapperBoth over encoding.OpenBlock(meta).Fetch/FetchSpans with " +
		"common.SearchOptions{StartPage,TotalPages}, then eval.Do(..., maxSeries) and " +
		"SeriesSet.ToProto(req). match_count is the SERIES count; the digest is the figure " +
		"the two implementations must agree on. The block is re-opened per fetch, so no " +
		"decoded page survives an iteration (FAIRNESS 15b)."
}

func (q *metricsQuerier) CountLabel() string { return "series" }

func (q *metricsQuerier) Search(query string, startRG, nRGs int, _ bool) (*searchOut, error) {
	var t timings
	ctx := context.Background()

	t0 := time.Now()
	req, err := buildQueryRangeRequest(q.blockDir, q.tenant, query, startRG, nRGs, q.mo)
	if err != nil {
		return nil, err
	}
	// Request construction reads meta.json; that is planning, not scanning.
	t.PlanNs = time.Since(t0).Nanoseconds()

	t0 = time.Now()
	resp, err := queryBlockMetrics(ctx, q.r, q.tenant, req)
	t.ExecuteNs = time.Since(t0).Nanoseconds()
	if err != nil {
		return nil, err
	}
	t.EngineTotalNs = t.PlanNs + t.ExecuteNs

	gt := newGroundTruth(req, resp)
	out := &searchOut{
		Count:   int64(gt.SeriesCount),
		Timings: t,
		Plan: fmt.Sprintf("queryBlock(query=%q start=%d end=%d step=%d maxSeries=%d startPage=%d pages=%d)",
			req.Query, req.Start, req.End, req.Step, req.MaxSeries, req.StartPage, req.PagesToSearch),
		InspectedBytes: gt.InspectedBytes,
		GroundTruth:    gt,
	}
	return out, nil
}

func (q *metricsQuerier) Explain(query string, startRG, nRGs int) (*explainOut, error) {
	t0 := time.Now()
	req, err := buildQueryRangeRequest(q.blockDir, q.tenant, query, startRG, nRGs, q.mo)
	if err != nil {
		return nil, err
	}
	expr, storageReqs, err := traceql.CompileFetchSpanRequests(req.Query)
	ns := time.Since(t0).Nanoseconds()
	if err != nil {
		return nil, err
	}
	return &explainOut{
		Plan: fmt.Sprintf("start=%d end=%d step=%d intervals=%d maxSeries=%d\n%s\n%+v",
			req.Start, req.End, req.Step, traceql.NewIntervalMapperFromReq(req).IntervalCount(),
			req.MaxSeries, expr.String(), storageReqs),
		Timings: timings{ParseNs: ns, EngineTotalNs: ns},
	}, nil
}
