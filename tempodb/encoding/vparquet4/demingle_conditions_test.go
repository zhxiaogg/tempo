package vparquet4

import (
	"testing"

	"github.com/grafana/tempo/pkg/traceql"
	"github.com/grafana/tempo/tempodb/backend"
	"github.com/stretchr/testify/require"
)

// keyTable lets a test declare which keys exist in which scopes.
type keyTable struct {
	span     map[string]bool
	resource map[string]bool
}

func (kt keyTable) hasKey(scope traceql.AttributeScope, key string) bool {
	switch scope {
	case traceql.AttributeScopeSpan:
		return kt.span[key]
	case traceql.AttributeScopeResource:
		return kt.resource[key]
	}
	return false
}

func unscoped(name string, op traceql.Operator, val string) traceql.Condition {
	return traceql.Condition{
		Attribute: traceql.NewScopedAttribute(traceql.AttributeScopeNone, false, name),
		Op:        op,
		Operands:  []traceql.Static{traceql.NewStaticString(val)},
	}
}

func TestDemingleUnscopedConditions(t *testing.T) {
	cases := []struct {
		name      string
		keys      keyTable
		input     []traceql.Condition
		wantScope []traceql.AttributeScope
	}{
		{
			name:      "span-only key gets demoted to span",
			keys:      keyTable{span: map[string]bool{"endpoint": true}},
			input:     []traceql.Condition{unscoped("endpoint", traceql.OpNotEqual, "x")},
			wantScope: []traceql.AttributeScope{traceql.AttributeScopeSpan},
		},
		{
			name:      "resource-only key gets demoted to resource",
			keys:      keyTable{resource: map[string]bool{"region": true}},
			input:     []traceql.Condition{unscoped("region", traceql.OpEqual, "us-east")},
			wantScope: []traceql.AttributeScope{traceql.AttributeScopeResource},
		},
		{
			name: "key on both sides stays unscoped",
			keys: keyTable{
				span:     map[string]bool{"foo": true},
				resource: map[string]bool{"foo": true},
			},
			input:     []traceql.Condition{unscoped("foo", traceql.OpEqual, "x")},
			wantScope: []traceql.AttributeScope{traceql.AttributeScopeNone},
		},
		{
			name:      "key on neither side stays unscoped",
			keys:      keyTable{},
			input:     []traceql.Condition{unscoped("ghost", traceql.OpEqual, "x")},
			wantScope: []traceql.AttributeScope{traceql.AttributeScopeNone},
		},
		{
			name: "intrinsics are untouched",
			keys: keyTable{},
			input: []traceql.Condition{{
				Attribute: traceql.NewIntrinsic(traceql.IntrinsicName),
				Op:        traceql.OpEqual,
				Operands:  []traceql.Static{traceql.NewStaticString("x")},
			}},
			wantScope: []traceql.AttributeScope{traceql.AttributeScopeNone},
		},
		{
			name: "already-scoped conditions are untouched",
			keys: keyTable{},
			input: []traceql.Condition{{
				Attribute: traceql.NewScopedAttribute(traceql.AttributeScopeSpan, false, "foo"),
				Op:        traceql.OpEqual,
				Operands:  []traceql.Static{traceql.NewStaticString("x")},
			}},
			wantScope: []traceql.AttributeScope{traceql.AttributeScopeSpan},
		},
		{
			name: "mixed batch demotes only the eligible ones",
			keys: keyTable{
				span:     map[string]bool{"endpoint": true},
				resource: map[string]bool{"cluster": true},
			},
			input: []traceql.Condition{
				unscoped("endpoint", traceql.OpNotEqual, "a"),
				unscoped("cluster", traceql.OpEqual, "prod"),
				unscoped("ambiguous", traceql.OpEqual, "x"), // neither -> stay unscoped
			},
			wantScope: []traceql.AttributeScope{
				traceql.AttributeScopeSpan,
				traceql.AttributeScopeResource,
				traceql.AttributeScopeNone,
			},
		},
		{
			name: "OpExists on span-only key gets demoted",
			keys: keyTable{span: map[string]bool{"k": true}},
			input: []traceql.Condition{{
				Attribute: traceql.NewScopedAttribute(traceql.AttributeScopeNone, false, "k"),
				Op:        traceql.OpExists,
			}},
			wantScope: []traceql.AttributeScope{traceql.AttributeScopeSpan},
		},
		{
			name: "OpNotExists on span-only key gets demoted",
			keys: keyTable{span: map[string]bool{"k": true}},
			input: []traceql.Condition{{
				Attribute: traceql.NewScopedAttribute(traceql.AttributeScopeNone, false, "k"),
				Op:        traceql.OpNotExists,
			}},
			wantScope: []traceql.AttributeScope{traceql.AttributeScopeSpan},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := demingleUnscopedConditions(tc.input, tc.keys.hasKey)
			require.Len(t, out, len(tc.input))
			for i := range out {
				require.Equal(t, tc.wantScope[i], out[i].Attribute.Scope, "condition %d", i)
				require.Equal(t, tc.input[i].Attribute.Name, out[i].Attribute.Name, "condition %d name", i)
				require.Equal(t, tc.input[i].Op, out[i].Op, "condition %d op", i)
			}
		})
	}
}

func TestContainsUnscopedNonIntrinsic(t *testing.T) {
	require.True(t, containsUnscopedNonIntrinsic([]traceql.Condition{
		unscoped("foo", traceql.OpEqual, "x"),
	}))
	require.False(t, containsUnscopedNonIntrinsic([]traceql.Condition{
		{
			Attribute: traceql.NewScopedAttribute(traceql.AttributeScopeSpan, false, "foo"),
			Op:        traceql.OpEqual,
			Operands:  []traceql.Static{traceql.NewStaticString("x")},
		},
	}))
	require.False(t, containsUnscopedNonIntrinsic([]traceql.Condition{
		{Attribute: traceql.NewIntrinsic(traceql.IntrinsicName), Op: traceql.OpEqual},
	}))
	require.False(t, containsUnscopedNonIntrinsic(nil))
	// Mixed list with one match -> true
	require.True(t, containsUnscopedNonIntrinsic([]traceql.Condition{
		{
			Attribute: traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, "scoped"),
			Op:        traceql.OpEqual,
			Operands:  []traceql.Static{traceql.NewStaticString("x")},
		},
		unscoped("unscoped", traceql.OpEqual, "y"),
	}))
}

func TestNewParquetHasKeyFn_DedicatedColumnsCount(t *testing.T) {
	dc := backend.DedicatedColumns{
		{Scope: backend.DedicatedColumnScopeSpan, Name: "endpoint", Type: backend.DedicatedColumnTypeString},
		{Scope: backend.DedicatedColumnScopeResource, Name: "region", Type: backend.DedicatedColumnTypeString},
	}

	hasKey := newParquetHasKeyFn(nil, nil, dc) // pf=nil, rgs=nil — exercises dedicated-only path

	require.True(t, hasKey(traceql.AttributeScopeSpan, "endpoint"))
	require.False(t, hasKey(traceql.AttributeScopeResource, "endpoint"))
	require.True(t, hasKey(traceql.AttributeScopeResource, "region"))
	require.False(t, hasKey(traceql.AttributeScopeSpan, "region"))
}
