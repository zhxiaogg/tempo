package frontend

import (
	"fmt"
	"net/http"

	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/tempodb/backend"
	"github.com/grafana/tempo/v3/tempodb/encoding"
)

// urlParamBlockFormat picks the block format of requests without TraceQL hints (trace by ID,
// tag names and values).
const urlParamBlockFormat = "blockFormat"

// resolveBlockFormat returns the block version a request reads: the block_format hint, else
// the blockFormat URL parameter, else def. "" means every version. hints and r may be nil.
func resolveBlockFormat(hints *traceql.Hints, r *http.Request, def string) (string, error) {
	v := def
	if s, ok := hints.Get(traceql.HintBlockFormat, traceql.TypeString, false); ok {
		v = s.EncodeToString(false)
	} else if r != nil {
		if p := r.URL.Query().Get(urlParamBlockFormat); p != "" {
			v = p
		}
	}
	if err := validateBlockFormat(v); err != nil {
		return "", err
	}
	return v, nil
}

// validateBlockFormat accepts "" and any known block version.
func validateBlockFormat(v string) error {
	if v == "" {
		return nil
	}
	if _, err := encoding.FromVersion(v); err != nil {
		return fmt.Errorf("invalid block format %q: %w", v, err)
	}
	return nil
}

// withBlockFormat keeps the blocks next keeps that are of the given version ("" = any).
func withBlockFormat(version string, next func(*backend.BlockMeta) bool) func(*backend.BlockMeta) bool {
	if version == "" {
		return next
	}
	return func(m *backend.BlockMeta) bool { return m.Version == version && next(m) }
}
