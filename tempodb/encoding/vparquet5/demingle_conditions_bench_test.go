package vparquet5

import (
	"testing"

	"github.com/grafana/tempo/pkg/traceql"
)

// BenchmarkDemingleUnscopedConditions_NoDemote measures the demingle pass when
// every unscoped key is present in both scopes (worst case for the rewrite —
// no condition is demoted, so the slice is rebuilt 1:1).
func BenchmarkDemingleUnscopedConditions_NoDemote(b *testing.B) {
	conds := []traceql.Condition{
		unscoped("a", traceql.OpEqual, "1"),
		unscoped("b", traceql.OpEqual, "2"),
		unscoped("c", traceql.OpEqual, "3"),
		unscoped("d", traceql.OpEqual, "4"),
	}
	hasKey := func(scope traceql.AttributeScope, key string) bool { return true }

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = demingleUnscopedConditions(conds, hasKey)
	}
}

// BenchmarkDemingleUnscopedConditions_AllDemote measures the happy path: every
// unscoped key is span-only, so each condition gets rewritten to span scope.
func BenchmarkDemingleUnscopedConditions_AllDemote(b *testing.B) {
	conds := []traceql.Condition{
		unscoped("a", traceql.OpEqual, "1"),
		unscoped("b", traceql.OpEqual, "2"),
		unscoped("c", traceql.OpEqual, "3"),
		unscoped("d", traceql.OpEqual, "4"),
	}
	hasKey := func(scope traceql.AttributeScope, key string) bool {
		return scope == traceql.AttributeScopeSpan
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = demingleUnscopedConditions(conds, hasKey)
	}
}

// BenchmarkContainsUnscopedNonIntrinsic_FastPath measures the early-out check
// used in createAllIterator to skip the demingle pass when nothing to rewrite.
// This runs on every fetch; it must be cheap.
func BenchmarkContainsUnscopedNonIntrinsic_FastPath(b *testing.B) {
	conds := []traceql.Condition{
		{Attribute: traceql.NewIntrinsic(traceql.IntrinsicName), Op: traceql.OpEqual},
		{Attribute: traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, "service.name"), Op: traceql.OpEqual},
		{Attribute: traceql.NewScopedAttribute(traceql.AttributeScopeSpan, false, "kind"), Op: traceql.OpEqual},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = containsUnscopedNonIntrinsic(conds)
	}
}
