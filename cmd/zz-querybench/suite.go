// -mode suite: the round-2 gRPC format benchmark client (GRPC_SPEC §4-§5), and
// -mode limitcheck: the in-process config-B investigation.
//
// The suite client is Tempo's own generated client (tempopb.NewQuerierClient /
// NewMetricsClient) driving ONE server through every configuration of a JSON suite file. For
// each query it sends 1 untimed warm-up and -iters timed requests, canonicalises every
// response (canon.go), and emits one JSON result per server. With -write-ref DIR it saves the
// canonical JSON of the warm-up response of each query as the reference; with -ref DIR it
// compares against that reference and records match plus the first difference.
//
// Every request carries gRPC metadata `zzqb-req: <config>/<query id>/<iteration>` (warm-up
// iteration "w"); the ordered list of tags is in the result's "requests" so the harness can
// join the server's `zzqb rpc=...` log lines to them.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/grafana/dskit/user"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/pkg/util"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/backend/local"
)

// ---------------------------------------------------------------- suite file

type suiteQuery struct {
	ID    string `json:"id"`
	Query string `json:"query"`
	StepS int    `json:"step_s,omitempty"`
}

type suiteConfig struct {
	ID              string       `json:"id"`
	Description     string       `json:"description,omitempty"`
	RPC             string       `json:"rpc"` // SearchBlock | QueryRange | FindTraceByID
	StartPage       int          `json:"start_page"`
	PagesToSearch   int          `json:"pages_to_search"`
	Limit           int          `json:"limit"`
	SpansPerSpanSet int          `json:"spans_per_spanset"`
	MaxSeries       int          `json:"max_series"`
	Exemplars       int          `json:"exemplars"`
	SampleIDs       string       `json:"sample_ids,omitempty"` // relative to the suite file
	Queries         []suiteQuery `json:"queries,omitempty"`
}

type suiteFile struct {
	Description string        `json:"description,omitempty"`
	Configs     []suiteConfig `json:"configs"`
}

func loadSuite(path string) (*suiteFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s suiteFile
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

func selectConfigs(s *suiteFile, csv string) ([]suiteConfig, error) {
	if csv == "" {
		return s.Configs, nil
	}
	var out []suiteConfig
	for _, id := range strings.Split(csv, ",") {
		id = strings.TrimSpace(id)
		found := false
		for _, c := range s.Configs {
			if c.ID == id {
				out = append(out, c)
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("config %q not in suite", id)
		}
	}
	return out, nil
}

func loadSampleIDs(suitePath, rel string) ([]string, error) {
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(suitePath), rel)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// ---------------------------------------------------------------- references

const gzipThreshold = 1 << 20

type refEntry struct {
	File    string `json:"file"`
	Digest  string `json:"digest"`
	Bytes   int    `json:"canonical_bytes"`
	Summary any    `json:"summary,omitempty"`
	// Corrected, when set, replaces Go's answer as the correctness reference because Go's
	// answer is provably wrong (GRPC_SPEC §8). The original entry stays for the record; the
	// diff note says which reference a mismatch was measured against.
	Corrected *refEntry `json:"corrected,omitempty"`
}

type refIndex struct {
	Note    string                         `json:"note"`
	Block   string                         `json:"block"`
	Configs map[string]map[string]refEntry `json:"configs"`
}

func readRefIndex(dir string) (*refIndex, error) {
	idx := &refIndex{Configs: map[string]map[string]refEntry{}}
	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if errors.Is(err, os.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, idx); err != nil {
		return nil, err
	}
	if idx.Configs == nil {
		idx.Configs = map[string]map[string]refEntry{}
	}
	return idx, nil
}

func writeRefFile(dir, cfg, name string, canon []byte) (string, error) {
	rel := filepath.Join(cfg, name+".json")
	data := canon
	if len(canon) > gzipThreshold {
		rel += ".gz"
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if _, err := zw.Write(canon); err != nil {
			return "", err
		}
		if err := zw.Close(); err != nil {
			return "", err
		}
		data = buf.Bytes()
	}
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	// Remove a stale sibling with the other extension so a reader never finds two.
	_ = os.Remove(strings.TrimSuffix(full, ".gz"))
	_ = os.Remove(strings.TrimSuffix(full, ".gz") + ".gz")
	return rel, os.WriteFile(full, data, 0o644)
}

func readRefFile(dir, rel string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(rel, ".gz") {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(zr)
	}
	return b, nil
}

// firstDiff walks two canonical JSON documents and describes the first difference.
func firstDiff(want, got []byte) string {
	var a, b any
	da := json.NewDecoder(bytes.NewReader(want))
	da.UseNumber()
	if err := da.Decode(&a); err != nil {
		return "reference does not parse: " + err.Error()
	}
	db := json.NewDecoder(bytes.NewReader(got))
	db.UseNumber()
	if err := db.Decode(&b); err != nil {
		return "response does not parse: " + err.Error()
	}
	if d, ok := walkDiff("$", a, b); ok {
		return d
	}
	return "canonical bytes differ but the documents are equal (should not happen)"
}

func short(v any) string {
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 240 {
		s = s[:240] + "..."
	}
	return s
}

func walkDiff(path string, a, b any) (string, bool) {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: want object %s, got %s", path, short(a), short(b)), true
		}
		keys := map[string]bool{}
		for k := range x {
			keys[k] = true
		}
		for k := range y {
			keys[k] = true
		}
		ks := make([]string, 0, len(keys))
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			av, aok := x[k]
			bv, bok := y[k]
			if !aok {
				return fmt.Sprintf("%s.%s: unexpected field, got %s", path, k, short(bv)), true
			}
			if !bok {
				return fmt.Sprintf("%s.%s: missing, want %s", path, k, short(av)), true
			}
			if d, ok := walkDiff(path+"."+k, av, bv); ok {
				return d, true
			}
		}
		return "", false
	case []any:
		y, ok := b.([]any)
		if !ok {
			return fmt.Sprintf("%s: want array, got %s", path, short(b)), true
		}
		n := len(x)
		if len(y) < n {
			n = len(y)
		}
		for i := 0; i < n; i++ {
			if d, ok := walkDiff(fmt.Sprintf("%s[%d]", path, i), x[i], y[i]); ok {
				if len(x) != len(y) {
					d = fmt.Sprintf("%s (array length want %d got %d)", d, len(x), len(y))
				}
				return d, true
			}
		}
		if len(x) != len(y) {
			if len(x) > len(y) {
				return fmt.Sprintf("%s: length want %d got %d; first missing element %s", path, len(x), len(y), short(x[n])), true
			}
			return fmt.Sprintf("%s: length want %d got %d; first extra element %s", path, len(x), len(y), short(y[n])), true
		}
		return "", false
	default:
		if fmt.Sprint(a) != fmt.Sprint(b) || fmt.Sprintf("%T", a) != fmt.Sprintf("%T", b) {
			return fmt.Sprintf("%s: want %s got %s", path, short(a), short(b)), true
		}
		return "", false
	}
}

// ---------------------------------------------------------------- results

type durStats struct {
	MinNs    int64   `json:"min_ns"`
	MedianNs int64   `json:"median_ns"`
	MaxNs    int64   `json:"max_ns"`
	P90Ns    int64   `json:"p90_ns,omitempty"`
	P99Ns    int64   `json:"p99_ns,omitempty"`
	MedianMs float64 `json:"median_ms"`
}

func pct(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	// nearest-rank
	r := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if r < 0 {
		r = 0
	}
	if r >= len(sorted) {
		r = len(sorted) - 1
	}
	return sorted[r]
}

func mkStats(ns []int64, withPct bool) durStats {
	var d durStats
	d.MinNs, d.MedianNs, d.MaxNs = stats(ns)
	d.MedianMs = float64(d.MedianNs) / 1e6
	if withPct {
		s := append([]int64(nil), ns...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		d.P90Ns, d.P99Ns = pct(s, 90), pct(s, 99)
	}
	return d
}

type queryResult struct {
	ID    string `json:"id"`
	Query string `json:"query"`
	StepS int    `json:"step_s,omitempty"`

	WarmupNs int64    `json:"warmup_ns"`
	IterNs   []int64  `json:"iter_ns"`
	Wall     durStats `json:"wall"`

	// proto.Size of the response (gogoproto Size()), i.e. the bytes on the wire minus framing.
	RespBytes int `json:"resp_bytes"`

	// search
	Traces         int    `json:"traces,omitempty"`
	SpanSets       int    `json:"spansets,omitempty"`
	Spans          int    `json:"spans,omitempty"`
	Matched        uint64 `json:"matched,omitempty"`
	TracesAtLimit  bool   `json:"traces_at_limit,omitempty"`
	SpansTruncated bool   `json:"spans_truncated,omitempty"`

	// metrics
	Series  int    `json:"series,omitempty"`
	Samples int    `json:"samples,omitempty"`
	SumAll  string `json:"sum_all,omitempty"`
	Status  string `json:"status,omitempty"`

	// reported by the server, not compared
	SearchMetrics *tempopb.SearchMetrics `json:"search_metrics,omitempty"`

	CanonicalBytes int      `json:"canonical_bytes"`
	Digest         string   `json:"digest"`
	Deterministic  bool     `json:"deterministic"`
	OtherDigests   []string `json:"other_digests,omitempty"`

	RefDigest string `json:"ref_digest,omitempty"`
	Match     *bool  `json:"match,omitempty"`
	FirstDiff string `json:"first_diff,omitempty"`

	Error string `json:"error,omitempty"`
}

type tbiID struct {
	ID        string   `json:"id"`
	Wall      durStats `json:"wall"`
	IterNs    []int64  `json:"iter_ns"`
	RespBytes int      `json:"resp_bytes"`
	Found     bool     `json:"found"`
	Spans     int      `json:"spans"`
	Digest    string   `json:"digest"`
	// Every timed lookup's canonical trace equals the warm-up's (the one compared with the
	// reference), so a timed iteration cannot be faster by being wrong.
	Deterministic bool   `json:"deterministic"`
	Match         *bool  `json:"match,omitempty"`
	FirstDiff     string `json:"first_diff,omitempty"`
	Error         string `json:"error,omitempty"`
}

type tbiResult struct {
	Lookups int `json:"lookups"`
	Found   int `json:"found"`
	// Distribution over the ids of each id's median-of-iters wall time.
	PerIDMedian durStats `json:"per_id_median"`
	// Distribution over every timed lookup (ids x iters).
	Pooled           durStats `json:"pooled"`
	RespBytesTotal   int64    `json:"resp_bytes_total"`
	SpansTotal       int      `json:"spans_total"`
	Matches          int      `json:"matches"`
	Mismatches       int      `json:"mismatches"`
	NoReference      int      `json:"no_reference"`
	Errors           int      `json:"errors"`
	NonDeterministic int      `json:"non_deterministic"`
	IDs              []tbiID  `json:"ids"`
}

type configResult struct {
	suiteConfig
	Results     []queryResult `json:"results,omitempty"`
	TraceByID   *tbiResult    `json:"trace_by_id,omitempty"`
	CorrectNote string        `json:"correctness_check"`
}

type reqRecord struct {
	Tag string `json:"tag"`
	RPC string `json:"rpc"`
}

type suiteResult struct {
	Mode    string          `json:"mode"`
	Server  string          `json:"server"`
	Addr    string          `json:"addr"`
	Suite   string          `json:"suite"`
	Block   string          `json:"block"`
	Iters   int             `json:"iters"`
	Ref     string          `json:"ref,omitempty"`
	Started string          `json:"started"`
	Configs []*configResult `json:"configs"`
	// Every request sent, in order (warm-ups and timed), for joining the server's log lines.
	Requests []reqRecord `json:"requests"`
	Env      env         `json:"env"`
}

// ---------------------------------------------------------------- the client

type suiteOpts struct {
	addr, blockDir, tenant, suitePath, configs, refDir, writeRef, server string
	iters                                                                int
}

func runSuite(o suiteOpts) (*suiteResult, error) {
	if o.iters < 1 {
		return nil, errors.New("-iters must be >= 1")
	}
	if o.suitePath == "" {
		return nil, errors.New("-suite is required")
	}
	if o.refDir != "" && o.writeRef != "" {
		return nil, errors.New("-ref and -write-ref are exclusive")
	}
	suite, err := loadSuite(o.suitePath)
	if err != nil {
		return nil, err
	}
	cfgs, err := selectConfigs(suite, o.configs)
	if err != nil {
		return nil, err
	}

	var ref *refIndex
	if o.refDir != "" {
		if ref, err = readRefIndex(o.refDir); err != nil {
			return nil, err
		}
	}
	var wref *refIndex
	if o.writeRef != "" {
		if wref, err = readRefIndex(o.writeRef); err != nil {
			return nil, err
		}
		wref.Note = "canonical Go (go-vp5) responses; see README.md in this directory for the formats"
		wref.Block = filepath.Base(o.blockDir)
	}

	cc, err := grpc.NewClient(o.addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256<<20)))
	if err != nil {
		return nil, err
	}
	defer cc.Close()
	qc := tempopb.NewQuerierClient(cc)
	mc := tempopb.NewMetricsClient(cc)

	baseCtx := user.InjectOrgID(context.Background(), o.tenant)
	baseCtx, err = user.InjectIntoGRPCRequest(baseCtx)
	if err != nil {
		return nil, err
	}

	res := &suiteResult{
		Mode: "suite", Server: o.server, Addr: o.addr, Suite: o.suitePath, Block: filepath.Base(o.blockDir),
		Iters: o.iters, Ref: o.refDir, Started: time.Now().UTC().Format(time.RFC3339), Env: currentEnv(),
	}

	tagCtx := func(tag, rpc string) context.Context {
		res.Requests = append(res.Requests, reqRecord{Tag: tag, RPC: rpc})
		return metadata.AppendToOutgoingContext(baseCtx, reqTagKey, tag)
	}

	// compare / record one canonical document
	check := func(cfgID, name string, canon []byte, digest string, summary any, match **bool, refDigest, diff *string) error {
		if wref != nil {
			rel, err := writeRefFile(o.writeRef, cfgID, name, canon)
			if err != nil {
				return err
			}
			if wref.Configs[cfgID] == nil {
				wref.Configs[cfgID] = map[string]refEntry{}
			}
			wref.Configs[cfgID][name] = refEntry{File: rel, Digest: digest, Bytes: len(canon), Summary: summary}
		}
		if ref != nil {
			e, ok := ref.Configs[cfgID][name]
			if !ok {
				return nil // no reference: match stays nil
			}
			note := ""
			// go-vp5 is always held to its own recorded answer; only the other servers are
			// checked against the corrected one.
			if e.Corrected != nil && o.server != "go-vp5" {
				e = *e.Corrected
				note = "(vs corrected reference, GRPC_SPEC §8) "
			}
			*refDigest = e.Digest
			m := e.Digest == digest
			*match = &m
			if !m {
				want, err := readRefFile(o.refDir, e.File)
				if err != nil {
					*diff = note + "cannot read reference: " + err.Error()
				} else {
					*diff = note + firstDiff(want, canon)
				}
			}
		}
		return nil
	}

	for _, cfg := range cfgs {
		cr := &configResult{suiteConfig: cfg}
		res.Configs = append(res.Configs, cr)
		fmt.Fprintf(os.Stderr, "zzqb suite: config %s (%s)\n", cfg.ID, cfg.RPC)

		switch cfg.RPC {
		case "SearchBlock":
			cr.CorrectNote = "exact equality of the canonical SearchResponse with the go-vp5 reference"
			for _, q := range cfg.Queries {
				qr := queryResult{ID: q.ID, Query: q.Query}
				req, err := buildSearchBlockRequest(o.blockDir, o.tenant, q.Query, cfg.StartPage, cfg.PagesToSearch, cfg.Limit, cfg.SpansPerSpanSet)
				if err != nil {
					return nil, err
				}
				digests := map[string]bool{}
				var first []byte
				for it := -1; it < o.iters; it++ {
					tag := fmt.Sprintf("%s/%s/%s", cfg.ID, q.ID, iterTag(it))
					ctx := tagCtx(tag, "SearchBlock")
					t0 := time.Now()
					resp, err := qc.SearchBlock(ctx, req)
					ns := time.Since(t0).Nanoseconds()
					if err != nil {
						qr.Error = err.Error()
						break
					}
					canon := mustJSON(canonSearch(resp))
					d := digestOf(canon)
					digests[d] = true
					if it < 0 {
						qr.WarmupNs = ns
						first = canon
						qr.Digest = d
						qr.CanonicalBytes = len(canon)
						qr.RespBytes = resp.Size()
						qr.Traces = len(resp.Traces)
						for _, tr := range resp.Traces {
							qr.SpanSets += len(tr.SpanSets)
							for _, ss := range tr.SpanSets {
								qr.Spans += len(ss.Spans)
								qr.Matched += uint64(ss.Matched)
								if len(ss.Spans) < int(ss.Matched) {
									qr.SpansTruncated = true
								}
							}
						}
						qr.TracesAtLimit = cfg.Limit > 0 && qr.Traces >= cfg.Limit
						qr.SearchMetrics = resp.Metrics
					} else {
						qr.IterNs = append(qr.IterNs, ns)
					}
				}
				finishQuery(&qr, digests)
				if first != nil {
					summary := map[string]any{"traces": qr.Traces, "spans": qr.Spans, "matched": qr.Matched, "resp_bytes": qr.RespBytes}
					if err := check(cfg.ID, q.ID, first, qr.Digest, summary, &qr.Match, &qr.RefDigest, &qr.FirstDiff); err != nil {
						return nil, err
					}
				}
				logQuery(cfg.ID, &qr)
				cr.Results = append(cr.Results, qr)
			}

		case "QueryRange":
			cr.CorrectNote = "exact equality of the canonical QueryRangeResponse (labels and every sample, bit-for-bit) with the go-vp5 reference"
			for _, q := range cfg.Queries {
				qr := queryResult{ID: q.ID, Query: q.Query, StepS: q.StepS}
				if q.StepS <= 0 {
					return nil, fmt.Errorf("config %s query %s: step_s must be > 0", cfg.ID, q.ID)
				}
				req, err := buildQueryRangeRequest(o.blockDir, o.tenant, q.Query, cfg.StartPage, cfg.PagesToSearch,
					metricsOpts{Step: time.Duration(q.StepS) * time.Second, MaxSeries: cfg.MaxSeries, Exemplars: cfg.Exemplars})
				if err != nil {
					return nil, err
				}
				digests := map[string]bool{}
				var first []byte
				for it := -1; it < o.iters; it++ {
					tag := fmt.Sprintf("%s/%s/%s", cfg.ID, q.ID, iterTag(it))
					ctx := tagCtx(tag, "QueryRange")
					t0 := time.Now()
					resp, err := mc.QueryRange(ctx, req)
					ns := time.Since(t0).Nanoseconds()
					if err != nil {
						qr.Error = err.Error()
						break
					}
					canon := mustJSON(canonQueryRange(resp))
					d := digestOf(canon)
					digests[d] = true
					if it < 0 {
						qr.WarmupNs = ns
						first = canon
						qr.Digest = d
						qr.CanonicalBytes = len(canon)
						qr.RespBytes = resp.Size()
						_, total, sumAll, _ := summarize(resp)
						qr.Series, qr.Samples, qr.SumAll = len(resp.Series), total, formatFloat(sumAll)
						qr.Status = resp.Status.String()
						qr.SearchMetrics = resp.Metrics
					} else {
						qr.IterNs = append(qr.IterNs, ns)
					}
				}
				finishQuery(&qr, digests)
				if first != nil {
					summary := map[string]any{"series": qr.Series, "samples": qr.Samples, "sum_all": qr.SumAll, "resp_bytes": qr.RespBytes}
					if err := check(cfg.ID, q.ID, first, qr.Digest, summary, &qr.Match, &qr.RefDigest, &qr.FirstDiff); err != nil {
						return nil, err
					}
				}
				logQuery(cfg.ID, &qr)
				cr.Results = append(cr.Results, qr)
			}

		case "FindTraceByID":
			cr.CorrectNote = "per id: exact equality of the canonical OTLP trace with the go-vp5 reference"
			ids, err := loadSampleIDs(o.suitePath, cfg.SampleIDs)
			if err != nil {
				return nil, err
			}
			tr := &tbiResult{Lookups: len(ids)}
			per := make([]tbiID, len(ids))
			raw := make([][]byte, len(ids))
			for i, id := range ids {
				per[i].ID = id
				b, err := util.HexStringToTraceID(id)
				if err != nil {
					return nil, fmt.Errorf("sample id %q: %w", id, err)
				}
				raw[i] = b
			}
			first := make([][]byte, len(ids))
			// Pass -1 is the untimed warm-up of every id; then -iters timed passes. Each id
			// thus gets 1 warm-up + iters timed lookups, interleaved with the other ids.
			for it := -1; it < o.iters; it++ {
				for i := range ids {
					if per[i].Error != "" {
						continue
					}
					tag := fmt.Sprintf("%s/%s/%s", cfg.ID, ids[i], iterTag(it))
					ctx := tagCtx(tag, "FindTraceByID")
					t0 := time.Now()
					resp, err := qc.FindTraceByID(ctx, &tempopb.TraceByIDRequest{TraceID: raw[i], QueryMode: "blocks"})
					ns := time.Since(t0).Nanoseconds()
					if err != nil {
						per[i].Error = err.Error()
						continue
					}
					canon := mustJSON(canonTraceByID(resp))
					if it >= 0 {
						if digestOf(canon) != per[i].Digest {
							per[i].Deterministic = false
						}
					}
					if it < 0 {
						first[i] = canon
						per[i].Digest = digestOf(canon)
						per[i].Deterministic = true
						per[i].RespBytes = resp.Size()
						per[i].Found = resp.Trace != nil
						if resp.Trace != nil {
							for _, rs := range resp.Trace.ResourceSpans {
								for _, ss := range rs.ScopeSpans {
									per[i].Spans += len(ss.Spans)
								}
							}
						}
					} else {
						per[i].IterNs = append(per[i].IterNs, ns)
					}
				}
			}
			var medians, pooled []int64
			for i := range per {
				p := &per[i]
				if p.Error != "" {
					tr.Errors++
					continue
				}
				p.Wall = mkStats(p.IterNs, false)
				medians = append(medians, p.Wall.MedianNs)
				pooled = append(pooled, p.IterNs...)
				tr.RespBytesTotal += int64(p.RespBytes)
				tr.SpansTotal += p.Spans
				if p.Found {
					tr.Found++
				}
				if !p.Deterministic {
					tr.NonDeterministic++
				}
				var refDigest string
				summary := map[string]any{"found": p.Found, "spans": p.Spans, "resp_bytes": p.RespBytes}
				if err := check(cfg.ID, p.ID, first[i], p.Digest, summary, &p.Match, &refDigest, &p.FirstDiff); err != nil {
					return nil, err
				}
				switch {
				case p.Match == nil:
					tr.NoReference++
				case *p.Match:
					tr.Matches++
				default:
					tr.Mismatches++
				}
			}
			tr.PerIDMedian = mkStats(medians, true)
			tr.Pooled = mkStats(pooled, true)
			tr.IDs = per
			cr.TraceByID = tr
			fmt.Fprintf(os.Stderr, "zzqb suite: %s lookups=%d found=%d spans=%d per-id median %.3fms p90 %.3fms p99 %.3fms match=%d mismatch=%d noref=%d errors=%d nondeterministic=%d\n",
				cfg.ID, tr.Lookups, tr.Found, tr.SpansTotal, float64(tr.PerIDMedian.MedianNs)/1e6, float64(tr.PerIDMedian.P90Ns)/1e6,
				float64(tr.PerIDMedian.P99Ns)/1e6, tr.Matches, tr.Mismatches, tr.NoReference, tr.Errors, tr.NonDeterministic)

		default:
			return nil, fmt.Errorf("config %s: unknown rpc %q", cfg.ID, cfg.RPC)
		}
	}

	if wref != nil {
		b, _ := json.MarshalIndent(wref, "", "  ")
		if err := os.WriteFile(filepath.Join(o.writeRef, "index.json"), append(b, '\n'), 0o644); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func iterTag(it int) string {
	if it < 0 {
		return "w"
	}
	return fmt.Sprint(it)
}

func finishQuery(qr *queryResult, digests map[string]bool) {
	if len(qr.IterNs) > 0 {
		qr.Wall = mkStats(qr.IterNs, false)
	}
	qr.Deterministic = len(digests) <= 1
	if !qr.Deterministic {
		for d := range digests {
			if d != qr.Digest {
				qr.OtherDigests = append(qr.OtherDigests, d)
			}
		}
		sort.Strings(qr.OtherDigests)
	}
}

func logQuery(cfg string, qr *queryResult) {
	m := "-"
	if qr.Match != nil {
		m = fmt.Sprint(*qr.Match)
	}
	if qr.Error != "" {
		fmt.Fprintf(os.Stderr, "zzqb suite: %s/%s ERROR %s\n", cfg, qr.ID, qr.Error)
		return
	}
	fmt.Fprintf(os.Stderr, "zzqb suite: %s/%s median %.2fms bytes=%d traces=%d spans=%d matched=%d series=%d samples=%d deterministic=%v match=%s %s\n",
		cfg, qr.ID, qr.Wall.MedianMs, qr.RespBytes, qr.Traces, qr.Spans, qr.Matched, qr.Series, qr.Samples, qr.Deterministic, m, qr.FirstDiff)
}

// ---------------------------------------------------------------- -mode limitcheck

// limitcheck answers, IN PROCESS (no gRPC, so no 256 MiB message cap), whether Go's limited
// SearchBlock answer (config B) is deterministic and equal to the first `limit` traces of the
// unlimited answer in block order (trace-id order; the block is sorted by trace id). Both
// answers go through the very same querierServer.SearchBlock handler the gRPC server
// registers, with the same SpansPerSpanSet, so the only difference is Limit.
type limitCheckQuery struct {
	ID    string `json:"id"`
	Query string `json:"query"`

	Runs           int      `json:"limited_runs"`
	LimitedDigests []string `json:"limited_digests"`
	Deterministic  bool     `json:"deterministic"`
	LimitedTraces  int      `json:"limited_traces"`
	LimitedMatched uint64   `json:"limited_matched"`
	LimitedNs      []int64  `json:"limited_ns"`

	FullTraces  int    `json:"full_traces"`
	FullSpans   int    `json:"full_spans_returned"`
	FullMatched uint64 `json:"full_matched"`
	FullNs      int64  `json:"full_ns"`

	FirstNDigest   string   `json:"first_n_digest"`
	EqualsFirstN   bool     `json:"equals_first_n_in_block_order"`
	SameTraceIDs   bool     `json:"same_trace_ids_as_first_n"`
	SubsetOfFull   bool     `json:"limited_is_subset_of_full"`
	FirstDiff      string   `json:"first_diff,omitempty"`
	LimitedTraceID []string `json:"limited_trace_ids"`
}

type limitCheckResult struct {
	Mode             string            `json:"mode"`
	Config           string            `json:"config"`
	StartPage        int               `json:"start_page"`
	PagesToSearch    int               `json:"pages_to_search"`
	Limit            int               `json:"limit"`
	SpansPerSpanSet  int               `json:"spans_per_spanset"`
	Queries          []limitCheckQuery `json:"queries"`
	AllDeterministic bool              `json:"all_deterministic"`
	AllEqualFirstN   bool              `json:"all_equal_first_n"`
	Env              env               `json:"env"`
}

func openQuerierServer(blockDir, tenant string) (*querierServer, error) {
	tenantDir := filepath.Dir(blockDir)
	if got := filepath.Base(tenantDir); got != tenant {
		return nil, fmt.Errorf("-tenant %q does not match the block's parent directory %q", tenant, got)
	}
	raw, _, _, err := local.New(&local.Config{Path: filepath.Dir(tenantDir)})
	if err != nil {
		return nil, err
	}
	rr := backend.NewReader(raw)
	blockID, err := uuid.Parse(filepath.Base(blockDir))
	if err != nil {
		return nil, err
	}
	meta, err := rr.BlockMeta(context.Background(), blockID, tenant)
	if err != nil {
		return nil, err
	}
	tbi, err := newBloomPassReader(rr)
	if err != nil {
		return nil, err
	}
	return &querierServer{r: rr, defaultTenant: tenant, engine: traceql.NewEngine(), meta: meta, tbiReader: tbi}, nil
}

func runLimitCheck(blockDir, tenant, suitePath, cfgID string, runs int) (*limitCheckResult, error) {
	suite, err := loadSuite(suitePath)
	if err != nil {
		return nil, err
	}
	cfgs, err := selectConfigs(suite, cfgID)
	if err != nil {
		return nil, err
	}
	cfg := cfgs[0]
	if cfg.RPC != "SearchBlock" || cfg.Limit <= 0 {
		return nil, fmt.Errorf("config %s is not a limited SearchBlock config", cfg.ID)
	}
	s, err := openQuerierServer(blockDir, tenant)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	out := &limitCheckResult{Mode: "limitcheck", Config: cfg.ID, StartPage: cfg.StartPage, PagesToSearch: cfg.PagesToSearch,
		Limit: cfg.Limit, SpansPerSpanSet: cfg.SpansPerSpanSet, AllDeterministic: true, AllEqualFirstN: true, Env: currentEnv()}

	for _, q := range cfg.Queries {
		lq := limitCheckQuery{ID: q.ID, Query: q.Query, Runs: runs}
		lreq, err := buildSearchBlockRequest(blockDir, tenant, q.Query, cfg.StartPage, cfg.PagesToSearch, cfg.Limit, cfg.SpansPerSpanSet)
		if err != nil {
			return nil, err
		}
		var limitedCanon []byte
		var limitedResp *tempopb.SearchResponse
		seen := map[string]bool{}
		for i := 0; i < runs; i++ {
			t0 := time.Now()
			resp, err := s.SearchBlock(ctx, lreq)
			lq.LimitedNs = append(lq.LimitedNs, time.Since(t0).Nanoseconds())
			if err != nil {
				return nil, err
			}
			c := canonSearch(resp)
			b := mustJSON(c)
			d := digestOf(b)
			if !seen[d] {
				seen[d] = true
				lq.LimitedDigests = append(lq.LimitedDigests, d)
			}
			if i == 0 {
				limitedCanon = b
				limitedResp = resp
				lq.LimitedTraces = len(c.Traces)
				for _, t := range c.Traces {
					lq.LimitedTraceID = append(lq.LimitedTraceID, t.TraceID)
					for _, ss := range t.SpanSets {
						lq.LimitedMatched += uint64(ss.Matched)
					}
				}
			}
		}
		lq.Deterministic = len(lq.LimitedDigests) == 1

		freq, err := buildSearchBlockRequest(blockDir, tenant, q.Query, cfg.StartPage, cfg.PagesToSearch, 0, cfg.SpansPerSpanSet)
		if err != nil {
			return nil, err
		}
		t0 := time.Now()
		full, err := s.SearchBlock(ctx, freq)
		lq.FullNs = time.Since(t0).Nanoseconds()
		if err != nil {
			return nil, err
		}
		lq.FullTraces = len(full.Traces)
		for _, t := range full.Traces {
			for _, ss := range t.SpanSets {
				lq.FullSpans += len(ss.Spans)
				lq.FullMatched += uint64(ss.Matched)
			}
		}
		// first N in block (= trace id) order
		trs := append([]*tempopb.TraceSearchMetadata(nil), full.Traces...)
		sort.Slice(trs, func(i, j int) bool { return padHex(trs[i].TraceID, 32) < padHex(trs[j].TraceID, 32) })
		n := cfg.Limit
		if n > len(trs) {
			n = len(trs)
		}
		firstN := mustJSON(canonSearch(&tempopb.SearchResponse{Traces: trs[:n]}))
		lq.FirstNDigest = digestOf(firstN)
		lq.EqualsFirstN = lq.FirstNDigest == lq.LimitedDigests[0]
		if !lq.EqualsFirstN {
			lq.FirstDiff = firstDiff(firstN, limitedCanon)
		}
		ids := map[string]bool{}
		for _, t := range trs[:n] {
			ids[t.TraceID] = true
		}
		lq.SameTraceIDs = len(lq.LimitedTraceID) == n
		for _, id := range lq.LimitedTraceID {
			if !ids[id] {
				lq.SameTraceIDs = false
			}
		}
		// subset: every limited trace (canonical) equals the full answer's trace of that id
		want := map[string]bool{}
		for _, t := range limitedResp.Traces {
			want[t.TraceID] = true
		}
		byID := map[string][]byte{}
		for _, t := range full.Traces {
			if want[t.TraceID] {
				byID[t.TraceID] = mustJSON(canonTraceMeta(t))
			}
		}
		lq.SubsetOfFull = true
		for _, t := range limitedResp.Traces {
			if string(byID[t.TraceID]) != string(mustJSON(canonTraceMeta(t))) {
				lq.SubsetOfFull = false
			}
		}
		if !lq.Deterministic {
			out.AllDeterministic = false
		}
		if !lq.EqualsFirstN {
			out.AllEqualFirstN = false
		}
		fmt.Fprintf(os.Stderr, "zzqb limitcheck: %s limited traces=%d deterministic=%v equals_first_%d=%v same_ids=%v subset=%v full traces=%d matched=%d %s\n",
			q.ID, lq.LimitedTraces, lq.Deterministic, n, lq.EqualsFirstN, lq.SameTraceIDs, lq.SubsetOfFull, lq.FullTraces, lq.FullMatched, lq.FirstDiff)
		out.Queries = append(out.Queries, lq)
		full = nil
	}
	return out, nil
}

// failures lists every answer that differs from its reference, errored, or changed between
// iterations.
func (r *suiteResult) failures() []string {
	var out []string
	for _, c := range r.Configs {
		for _, q := range c.Results {
			switch {
			case q.Error != "":
				out = append(out, c.ID+"/"+q.ID+": error "+q.Error)
			case q.Match != nil && !*q.Match:
				out = append(out, c.ID+"/"+q.ID+": "+q.FirstDiff)
			case !q.Deterministic:
				out = append(out, c.ID+"/"+q.ID+": nondeterministic")
			}
		}
		if t := c.TraceByID; t != nil && (t.Mismatches > 0 || t.Errors > 0 || t.NonDeterministic > 0) {
			out = append(out, fmt.Sprintf("%s: %d mismatches, %d errors, %d nondeterministic", c.ID, t.Mismatches, t.Errors, t.NonDeterministic))
		}
	}
	return out
}

// waitTCP waits until addr accepts a TCP connection (0 = don't wait).
func waitTCP(addr string, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	deadline := time.Now().Add(d)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not reachable after %s: %w", addr, d, err)
		}
		time.Sleep(time.Second)
	}
}
