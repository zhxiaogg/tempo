package querier

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRustEngineWants(t *testing.T) {
	e := &rustEngine{}
	for _, tc := range []struct {
		query, version string
		want           bool
	}{
		{`{ span.a = "x" } with(querier_engine="rust")`, "vCG1", true},
		{`{ span.a = "x" } with(block_format="vCG1", querier_engine="rust")`, "vCG1", true},
		{`{ span.a = "x" } | rate() with(querier_engine="rust")`, "vCG1", true},
		{`{ span.a = "x" } with(querier_engine="rust")`, "vParquet5", false}, // the sidecar reads vCG1 only
		{`{ span.a = "x" } with(querier_engine="go")`, "vCG1", false},
		{`{ span.a = "x" }`, "vCG1", false},
		{`{ span.a = `, "vCG1", false}, // unparsable: Go reports the error
	} {
		require.Equal(t, tc.want, e.wants(tc.query, tc.version), tc.query)
	}
	var none *rustEngine // no sidecar configured
	require.False(t, none.wants(`{ span.a = "x" } with(querier_engine="rust")`, "vCG1"))
}

func TestNewRustEngineDisabled(t *testing.T) {
	e, err := newRustEngine(RustEngineConfig{}, 1<<20)
	require.NoError(t, err)
	require.Nil(t, e)
}

func TestRustEngineFallback(t *testing.T) {
	e := &rustEngine{}
	ctx := context.Background()
	require.True(t, e.fallback(ctx, "search", status.Error(codes.Unimplemented, "spanset expression")))
	require.True(t, e.fallback(ctx, "search", status.Error(codes.Unavailable, "sidecar down")))
	require.False(t, e.fallback(ctx, "search", status.Error(codes.Canceled, "canceled")))
	require.False(t, e.fallback(ctx, "search", context.DeadlineExceeded))
	done, cancel := context.WithCancel(ctx)
	cancel()
	require.False(t, e.fallback(done, "search", status.Error(codes.Unavailable, "after cancel")))
}
