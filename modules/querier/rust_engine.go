package querier

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/user"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/grafana/tempo/v3/pkg/tempopb"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/pkg/util/log"
	"github.com/grafana/tempo/v3/tempodb/encoding/vcg1"
)

const (
	engineGo   = "go"
	engineRust = "rust"
)

// RustEngineConfig points the querier at a tempo-rs sidecar (`rustquerier serve-tempo`), which
// runs the vCG1 search and metrics block jobs of queries with the querier_engine="rust" hint.
type RustEngineConfig struct {
	// Address is the sidecar's gRPC address, e.g. 127.0.0.1:7777. Empty disables the engine.
	Address string `yaml:"address"`
}

var metricRustEngineFallbacks = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "tempo",
	Name:      "querier_rust_engine_fallbacks_total",
	Help:      "Block jobs asked to run on the Rust engine that ran on the Go engine instead, by operation and gRPC code.",
}, []string{"operation", "code"})

// rustEngine is the client of the tempo-rs sidecar; nil when none is configured.
type rustEngine struct {
	search  tempopb.QuerierClient
	metrics tempopb.MetricsClient
}

func newRustEngine(cfg RustEngineConfig, maxMsgSize int) (*rustEngine, error) {
	if cfg.Address == "" {
		return nil, nil
	}
	conn, err := grpc.NewClient(cfg.Address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMsgSize)),
	)
	if err != nil {
		return nil, fmt.Errorf("rust engine client %s: %w", cfg.Address, err)
	}
	return &rustEngine{search: tempopb.NewQuerierClient(conn), metrics: tempopb.NewMetricsClient(conn)}, nil
}

// wants reports whether a block job of this query and block version asks for the Rust engine.
func (e *rustEngine) wants(query, version string) bool {
	if e == nil || version != vcg1.VersionString {
		return false
	}
	expr, err := traceql.ParseNoOptimizations(query)
	if err != nil || expr.Hints == nil {
		return false
	}
	v, ok := expr.Hints.Get(traceql.HintQuerierEngine, traceql.TypeString, true)
	return ok && v.EncodeToString(false) == engineRust
}

// fallback reports whether a job the Rust engine did not answer runs on Go: not when the job
// itself was canceled or timed out (the frontend cancels a search's other jobs once it has
// enough results), which the caller returns as is.
func (e *rustEngine) fallback(ctx context.Context, op string, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch status.Code(err) {
	case codes.Canceled, codes.DeadlineExceeded:
		return false
	}
	metricRustEngineFallbacks.WithLabelValues(op, status.Code(err).String()).Inc()
	level.Debug(log.Logger).Log("msg", "rust engine fallback", "op", op, "err", err)
	return true
}

func (e *rustEngine) searchBlock(ctx context.Context, req *tempopb.SearchBlockRequest) (*tempopb.SearchResponse, error) {
	ctx, err := user.InjectIntoGRPCRequest(ctx)
	if err != nil {
		return nil, err
	}
	return e.search.SearchBlock(ctx, req)
}

func (e *rustEngine) queryRange(ctx context.Context, req *tempopb.QueryRangeRequest) (*tempopb.QueryRangeResponse, error) {
	ctx, err := user.InjectIntoGRPCRequest(ctx)
	if err != nil {
		return nil, err
	}
	return e.metrics.QueryRange(ctx, req)
}
