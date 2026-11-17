package querier

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/v3/pkg/tempopb"
)

func TestTraceByIDBlocksVersion(t *testing.T) {
	blocks := func(vs ...string) *tempopb.TraceByIDBlocks {
		b := &tempopb.TraceByIDBlocks{}
		for _, v := range vs {
			b.Blocks = append(b.Blocks, &tempopb.TraceByIDBlock{Version: v})
		}
		return b
	}
	require.Equal(t, "none", traceByIDBlocksVersion(nil))
	require.Equal(t, "none", traceByIDBlocksVersion(blocks()))
	require.Equal(t, "vCG1", traceByIDBlocksVersion(blocks("vCG1", "vCG1")))
	require.Equal(t, "mixed", traceByIDBlocksVersion(blocks("vCG1", "vParquet5")))
}
