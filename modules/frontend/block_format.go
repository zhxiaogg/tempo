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
	if h := blockFormatHint(hints); h != nil {
		// a hint must name a format: an empty or non-string one would read every version
		if h.Value.Type != traceql.TypeString || h.Value.EncodeToString(false) == "" {
			return "", fmt.Errorf("invalid %s hint %s: want a block version string", traceql.HintBlockFormat, h.Value.EncodeToString(true))
		}
		v = h.Value.EncodeToString(false)
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

// blockFormatHint returns the block_format hint, of any type, or nil.
func blockFormatHint(hints *traceql.Hints) *traceql.Hint {
	if hints == nil {
		return nil
	}
	for _, h := range hints.Hints {
		if h.Name == traceql.HintBlockFormat {
			return h
		}
	}
	return nil
}
