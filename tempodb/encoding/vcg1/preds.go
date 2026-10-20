package vcg1

// Condition checks and predicate construction are vparquet5's (block_traceql.go,
// coalesce_conditions.go), so every value is tested by exactly the predicate Tempo builds.

import (
	"fmt"
	"math"
	"reflect"

	"github.com/grafana/tempo/v3/pkg/parquetquery"
	"github.com/grafana/tempo/v3/pkg/traceql"
	"github.com/grafana/tempo/v3/pkg/util"
	"github.com/grafana/tempo/v3/tempodb/encoding/vparquet5"
)

func checkConditions(conditions []traceql.Condition) error {
	for _, cond := range conditions {
		opCount := len(cond.Operands)

		switch cond.Op {

		case traceql.OpNone, traceql.OpExists:
			if opCount != 0 {
				return fmt.Errorf("operation %v must have 0 arguments. condition: %+v", cond.Op, cond)
			}

		case traceql.OpEqual, traceql.OpNotEqual,
			traceql.OpGreater, traceql.OpGreaterEqual,
			traceql.OpLess, traceql.OpLessEqual,
			traceql.OpRegex, traceql.OpNotRegex,
			traceql.OpIn, traceql.OpNotIn,
			traceql.OpRegexMatchAny, traceql.OpRegexMatchNone:
			if opCount != 1 {
				return fmt.Errorf("operation %v must have exactly 1 argument. condition: %+v", cond.Op, cond)
			}

		case traceql.OpNotExists:
			if opCount != 0 {
				return fmt.Errorf("operation %v must have 0 arguments. condition: %+v", cond.Op, cond)
			}
			if cond.Attribute.Intrinsic != traceql.IntrinsicNone {
				return fmt.Errorf("intrinsics cannot be = nil")
			}
			if cond.Attribute == traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, "service.name") {
				return fmt.Errorf("resource.service.name cannot be = nil")
			}

		default:
			return fmt.Errorf("unknown operation. condition: %+v", cond)
		}

		// Verify all operands are of the same type
		if opCount == 0 {
			continue
		}

		for i := 1; i < opCount; i++ {
			if reflect.TypeOf(cond.Operands[0]) != reflect.TypeOf(cond.Operands[i]) {
				return fmt.Errorf("operands must be of the same type. condition: %+v", cond)
			}
		}
	}

	return nil
}

func operandType(operands traceql.Operands) traceql.StaticType {
	if len(operands) > 0 {
		return operands[0].Type
	}
	return traceql.TypeNil
}

func isMatchingColumnType(colType, opType traceql.StaticType) bool {
	if colType == opType {
		return true
	}
	switch opType {
	case traceql.TypeStringArray:
		return colType == traceql.TypeString
	case traceql.TypeIntArray:
		return colType == traceql.TypeInt
	case traceql.TypeFloatArray:
		return colType == traceql.TypeFloat
	case traceql.TypeBooleanArray:
		return colType == traceql.TypeBoolean
	default:
		return false
	}
}

// spansetIterator turns the parquet iterator into the final
func createExistencePredicate(op traceql.Operator) (pred parquetquery.Predicate, handled bool) {
	switch op {
	case traceql.OpNone:
		return nil, true
	case traceql.OpExists:
		return parquetquery.NewSkipNilsPredicate(), true
	case traceql.OpNotExists:
		return parquetquery.NewNilValuePredicate(), true
	default:
		return nil, false
	}
}

func createPredicate(op traceql.Operator, operands traceql.Operands) (parquetquery.Predicate, error) {
	if pred, handled := createExistencePredicate(op); handled {
		return pred, nil
	}

	switch operands[0].Type {
	case traceql.TypeString, traceql.TypeStringArray:
		return createStringPredicate(op, operands)
	case traceql.TypeInt, traceql.TypeIntArray:
		return createIntPredicate(op, operands)
	case traceql.TypeFloat, traceql.TypeFloatArray:
		return createFloatPredicate(op, operands)
	case traceql.TypeBoolean, traceql.TypeBooleanArray:
		return createBoolPredicate(op, operands)
	default:
		return nil, fmt.Errorf("cannot create predicate for operand: %v", operands[0])
	}
}

func createStringPredicate(op traceql.Operator, operands traceql.Operands) (parquetquery.Predicate, error) {
	if pred, handled := createExistencePredicate(op); handled {
		return pred, nil
	}

	switch operands[0].Type {
	case traceql.TypeString:
		s := operands[0].EncodeToString(false)

		switch op {
		case traceql.OpEqual:
			return parquetquery.NewStringEqualPredicate(s), nil
		case traceql.OpNotEqual:
			return parquetquery.NewStringNotEqualPredicate(s), nil
		case traceql.OpRegex:
			return parquetquery.NewRegexInPredicate([]string{s})
		case traceql.OpNotRegex:
			return parquetquery.NewRegexNotInPredicate([]string{s})
		case traceql.OpGreater:
			return parquetquery.NewStringGreaterPredicate([]byte(s)), nil
		case traceql.OpGreaterEqual:
			return parquetquery.NewStringGreaterEqualPredicate([]byte(s)), nil
		case traceql.OpLess:
			return parquetquery.NewStringLessPredicate([]byte(s)), nil
		case traceql.OpLessEqual:
			return parquetquery.NewStringLessEqualPredicate([]byte(s)), nil
		default:
			return nil, fmt.Errorf("operator not supported for strings: %+v", op)
		}
	case traceql.TypeStringArray:
		strs, _ := operands[0].StringArray()
		switch op {
		case traceql.OpEqual, traceql.OpIn:
			return parquetquery.NewStringInPredicate(strs), nil
		case traceql.OpNotEqual, traceql.OpNotIn:
			return parquetquery.NewStringNotInPredicate(strs), nil
		case traceql.OpRegex, traceql.OpRegexMatchAny:
			return parquetquery.NewRegexInPredicate(strs)
		case traceql.OpNotRegex, traceql.OpRegexMatchNone:
			return parquetquery.NewRegexNotInPredicate(strs)
		default:
			return nil, fmt.Errorf("operator not supported for strings arrays: %+v", op)
		}
	default:
		return nil, fmt.Errorf("operand is not a string or string array: %s", operands[0])
	}
}

func createBytesPredicate(op traceql.Operator, operands traceql.Operands, isSpan bool) (parquetquery.Predicate, error) {
	if pred, handled := createExistencePredicate(op); handled {
		return pred, nil
	}

	switch operands[0].Type {
	case traceql.TypeString:
		s := operands[0].EncodeToString(false)

		var id []byte
		var err error
		if isSpan {
			id, err = util.HexStringToSpanID(s)
		} else {
			id, err = util.HexStringToTraceID(s)
		}

		if err != nil {
			return nil, nil
		}

		switch op {
		case traceql.OpEqual:
			return parquetquery.NewByteEqualPredicate(id), nil
		case traceql.OpNotEqual:
			return parquetquery.NewByteNotEqualPredicate(id), nil
		default:
			return nil, fmt.Errorf("operator not supported for IDs: %+v", op)
		}
	case traceql.TypeStringArray:
		strs, _ := operands[0].StringArray()
		ids := make([][]byte, 0, len(strs))

		for _, s := range strs {
			var id []byte
			var err error
			if isSpan {
				id, err = util.HexStringToSpanID(s)
			} else {
				id, err = util.HexStringToTraceID(s)
			}

			if err != nil {
				return nil, nil
			}
			ids = append(ids, id)
		}

		switch op {
		case traceql.OpEqual, traceql.OpIn:
			return parquetquery.NewByteInPredicate(ids), nil
		case traceql.OpNotEqual, traceql.OpNotIn:
			return parquetquery.NewByteNotInPredicate(ids), nil
		default:
			return nil, fmt.Errorf("operator not supported for IDs: %+v", op)
		}
	default:
		return nil, fmt.Errorf("operand is not string or string array: %s", operands[0])
	}
}

func createDurationPredicate(op traceql.Operator, operands traceql.Operands) (parquetquery.Predicate, error) {
	if pred, handled := createExistencePredicate(op); handled {
		return pred, nil
	}

	if len(operands) == 0 {
		return nil, fmt.Errorf("operands cannot be empty")
	}

	if operands[0].Type == traceql.TypeFloat || operands[0].Type == traceql.TypeFloatArray {
		// The column is already indexed as int, so we need to convert the float to int
		return createIntPredicateFromFloat(op, operands)
	}

	return createIntPredicate(op, operands)
}

// createIntPredicateFromFloat adapts a float-based query operand to an int column.
// If the float is exactly representable as an int64 (e.g. 42.0), we compare the
// column to that integer. Otherwise, if the float is non-integer or out of the
// int64 range, we return a "trivial" outcome:
//
//   - "=" on a non-integer float returns nil, meaning "no filter"
//   - "!=" on a non-integer float always matches, implemented as a predicate
//     that returns true for every row.
//   - For "<", "<=", ">", ">=", we shift the boundary to the nearest integer.
//     For example, "x < 10.3" becomes "x <= 10" for the int column.
//
// Note: If returning nil, no column-level filtering is applied for this condition.
func createIntPredicateFromFloat(op traceql.Operator, operands traceql.Operands) (parquetquery.Predicate, error) {
	if pred, handled := createExistencePredicate(op); handled {
		return pred, nil
	}

	if len(operands) == 0 {
		return nil, fmt.Errorf("operands cannot be empty")
	}

	switch operands[0].Type {
	case traceql.TypeFloat:
		f := operands[0].Float()

		if math.IsNaN(f) {
			return nil, nil
		}

		// Check if it's in [MinInt64, MaxInt64) range, and if so, see if it's an integer.
		if float64(math.MinInt64) <= f && f < float64(math.MaxInt64) {
			if intPart, frac := math.Modf(f); frac == 0 {
				intOperands := traceql.Operands{traceql.NewStaticInt(int(intPart))}
				return createIntPredicate(op, intOperands)
			}
		}

		switch op {
		case traceql.OpEqual:
			return nil, nil
		case traceql.OpNotEqual:
			return parquetquery.NewCallbackPredicate(func() bool { return true }), nil
		case traceql.OpGreater, traceql.OpGreaterEqual:
			switch {
			case f < float64(math.MinInt64):
				return parquetquery.NewCallbackPredicate(func() bool { return true }), nil
			case float64(math.MaxInt64) <= f:
				return nil, nil
			case 0 < f:
				// "x > 10.3" -> "x >= 11"
				return parquetquery.NewIntGreaterEqualPredicate(int64(f) + 1), nil
			default:
				// "x > -2.7" -> "x >= -2"
				return parquetquery.NewIntGreaterEqualPredicate(int64(f)), nil
			}
		case traceql.OpLess, traceql.OpLessEqual:
			switch {
			case f < float64(math.MinInt64):
				return nil, nil
			case float64(math.MaxInt64) <= f:
				return parquetquery.NewCallbackPredicate(func() bool { return true }), nil
			case f < 0:
				// "x < -2.7" -> "x <= -3"
				return parquetquery.NewIntLessEqualPredicate(int64(f) - 1), nil
			default:
				// "x < 10.3" -> "x <= 10"
				return parquetquery.NewIntLessEqualPredicate(int64(f)), nil
			}
		default:
			return nil, fmt.Errorf("operator not supported for floats: %+v", op)
		}
	case traceql.TypeFloatArray:
		floats, _ := operands[0].FloatArray()
		ints := make([]int64, 0, len(floats))
		for _, f := range floats {
			if math.IsNaN(f) {
				continue
			}

			// Check if it's in [MinInt64, MaxInt64) range, and if so, see if it's an integer.
			if float64(math.MinInt64) <= f && f < float64(math.MaxInt64) {
				if intPart, frac := math.Modf(f); frac == 0 {
					ints = append(ints, int64(intPart))
				}
			}
		}
		switch op {
		case traceql.OpEqual, traceql.OpIn:
			if len(ints) == 0 {
				return nil, nil
			}
			return parquetquery.NewIntInPredicate(ints), nil
		case traceql.OpNotEqual, traceql.OpNotIn:
			if len(ints) == 0 {
				return parquetquery.NewCallbackPredicate(func() bool { return true }), nil
			}
			return parquetquery.NewIntNotInPredicate(ints), nil
		default:
			return nil, fmt.Errorf("operator not supported for float arrays: %+v", op)
		}
	default:
		return nil, fmt.Errorf("operand is not float or float array: %s", operands[0].EncodeToString(false))
	}
}

func createIntPredicate(op traceql.Operator, operands traceql.Operands) (parquetquery.Predicate, error) {
	if pred, handled := createExistencePredicate(op); handled {
		return pred, nil
	}

	if len(operands) == 0 {
		return nil, fmt.Errorf("operands cannot be empty")
	}

	var i int64
	switch operands[0].Type {
	case traceql.TypeInt:
		n, _ := operands[0].Int()
		i = int64(n)
	case traceql.TypeDuration:
		d, _ := operands[0].Duration()
		i = d.Nanoseconds()
	case traceql.TypeStatus:
		st, _ := operands[0].Status()
		i = int64(vparquet5.StatusCodeMapping[st.String()])
	case traceql.TypeKind:
		k, _ := operands[0].Kind()
		i = int64(vparquet5.KindMapping[k.String()])
	case traceql.TypeIntArray:
		ints, _ := operands[0].Int64Array()

		switch op {
		case traceql.OpEqual, traceql.OpIn:
			return parquetquery.NewIntInPredicate(ints), nil
		case traceql.OpNotEqual, traceql.OpNotIn:
			return parquetquery.NewIntNotInPredicate(ints), nil
		default:
			return nil, fmt.Errorf("operator not supported for int arrays: %+v", op)
		}
	default:
		return nil, fmt.Errorf("operand is not int, int array, duration, status or kind: %s", operands[0].EncodeToString(false))
	}

	switch op {
	case traceql.OpEqual:
		return parquetquery.NewIntEqualPredicate(i), nil
	case traceql.OpNotEqual:
		return parquetquery.NewIntNotEqualPredicate(i), nil
	case traceql.OpGreater:
		return parquetquery.NewIntGreaterPredicate(i), nil
	case traceql.OpGreaterEqual:
		return parquetquery.NewIntGreaterEqualPredicate(i), nil
	case traceql.OpLess:
		return parquetquery.NewIntLessPredicate(i), nil
	case traceql.OpLessEqual:
		return parquetquery.NewIntLessEqualPredicate(i), nil
	default:
		return nil, fmt.Errorf("operator not supported for integers: %+v", op)
	}
}

func createFloatPredicate(op traceql.Operator, operands traceql.Operands) (parquetquery.Predicate, error) {
	if pred, handled := createExistencePredicate(op); handled {
		return pred, nil
	}

	switch operands[0].Type {
	case traceql.TypeFloat:
		f := operands[0].Float()

		switch op {
		case traceql.OpEqual:
			return parquetquery.NewFloatEqualPredicate(f), nil
		case traceql.OpNotEqual:
			return parquetquery.NewFloatNotEqualPredicate(f), nil
		case traceql.OpGreater:
			return parquetquery.NewFloatGreaterPredicate(f), nil
		case traceql.OpGreaterEqual:
			return parquetquery.NewFloatGreaterEqualPredicate(f), nil
		case traceql.OpLess:
			return parquetquery.NewFloatLessPredicate(f), nil
		case traceql.OpLessEqual:
			return parquetquery.NewFloatLessEqualPredicate(f), nil
		default:
			return nil, fmt.Errorf("operator not supported for floats: %+v", op)
		}
	case traceql.TypeFloatArray:
		floats, _ := operands[0].FloatArray()

		switch op {
		case traceql.OpEqual, traceql.OpIn:
			return parquetquery.NewFloatInPredicate(floats), nil
		case traceql.OpNotEqual, traceql.OpNotIn:
			return parquetquery.NewFloatNotInPredicate(floats), nil
		default:
			return nil, fmt.Errorf("operator not supported for float arrays: %+v", op)
		}
	default:
		return nil, fmt.Errorf("operand is not float or float array: %s", operands[0].EncodeToString(false))
	}
}

func createBoolPredicate(op traceql.Operator, operands traceql.Operands) (parquetquery.Predicate, error) {
	if pred, handled := createExistencePredicate(op); handled {
		return pred, nil
	}

	switch operands[0].Type {
	case traceql.TypeBoolean:
		b, _ := operands[0].Bool()

		switch op {
		case traceql.OpEqual:
			return parquetquery.NewBoolEqualPredicate(b), nil
		case traceql.OpNotEqual:
			return parquetquery.NewBoolNotEqualPredicate(b), nil
		default:
			return nil, fmt.Errorf("operator not supported for booleans: %+v", op)
		}
	case traceql.TypeBooleanArray:
		bools, _ := operands[0].BooleanArray()

		switch op {
		case traceql.OpEqual, traceql.OpIn:
			return parquetquery.NewBoolInPredicate(bools), nil
		case traceql.OpNotEqual, traceql.OpNotIn:
			return parquetquery.NewBoolNotInPredicate(bools), nil
		default:
			return nil, fmt.Errorf("operator not supported for boolean arrays: %+v", op)
		}
	default:
		return nil, fmt.Errorf("oparand is not bool or bool array: %+v", operands[0].EncodeToString(false))
	}
}
func orIfNeeded(preds []parquetquery.Predicate) parquetquery.Predicate {
	switch len(preds) {
	case 0:
		return nil
	case 1:
		return preds[0]
	default:
		return parquetquery.NewOrPredicate(preds...)
	}
}

// coalesceConditions reduces the amount of data pulled from the backend when the same column is used in multiple conditions
//
//	todo: There is a lot that can be done with this method. for example we can coalesce conditions if they have equivalent
//	operands. consider a query like: { span.foo >= 1 } && { span.foo = 1 } the conditions could be coalesced to only
//	the >= condition. it also is currently ignoring situations where allconditions is true, but some of these
//	improvements apply to that case as well.
func coalesceConditions(f *traceql.FetchSpansRequest) {
	// only do this if all conditions is false for now. it's safer and all conditions queries tend to be quite quick
	if !f.AllConditions {
		prevLen := 0
		for len(f.Conditions) != prevLen { // check combinations until we can't coalesce any more
			prevLen = len(f.Conditions)

			// search for conditions with the same attribute name and consider coalescing them
			for i := 0; i < len(f.Conditions); i++ {
				for j := i + 1; j < len(f.Conditions); j++ {
					if c, ok := coalesce(f.Conditions[i], f.Conditions[j]); ok {
						f.Conditions[i] = c
						f.Conditions = append(f.Conditions[:j], f.Conditions[j+1:]...)
						j--
					}
				}
			}
		}
	}
}

// coalesce takes two conditions and turns them into one. it returns a bool to indicate
// if the returned condition is valid or if it should just continue using the original 2 conditions
//
// it is very difficult to coalesce conditions in a way that will always be more performant at the fetch layer
// consider: { resource.service.name = "foo" || resource.service.name = "bar" }
// if foo and bar are rare then it will be faster to double pull the column and intersect the results. if
// foo and bar are common then it will be faster to single pull the column and allow the engine to do the work.
// currently the fetch layer does not support the = condition with multiple operands. we should add capability
// and then extend this method.
//
// therefore! we will only coalesce conditions in the following cases:
//   - they are exactly the same
//   - they will pull every span anyway. example: { span.foo = "bar" } >> { span.foo != "bar" }
func coalesce(c1, c2 traceql.Condition) (traceql.Condition, bool) {
	// if the conditions are exactly the same then we can just return one of them
	if c1.Attribute == c2.Attribute &&
		c1.Op == c2.Op &&
		operandsEqual(c1, c2) {
		return c1, true
	}

	// if the operations are != and = and the operands are the same this is going to pull every row. let's
	// collapse to one condition with OpNone and no operands
	if c1.Attribute == c2.Attribute && // attributes equal
		(c1.Op == traceql.OpEqual && c2.Op == traceql.OpNotEqual || c1.Op == traceql.OpNotEqual && c2.Op == traceql.OpEqual) && // operands are != and =
		operandsEqual(c1, c2) { // operands equal
		return traceql.Condition{Attribute: c1.Attribute, Op: traceql.OpNone, Operands: nil}, true
	}

	// if one of the operations is opnone we're already pulling every row. coalesce
	if c1.Attribute == c2.Attribute && // attributes equal
		(c1.Op == traceql.OpNone || c2.Op == traceql.OpNone) { // one operand is opnone
		return traceql.Condition{Attribute: c1.Attribute, Op: traceql.OpNone, Operands: nil}, true
	}

	return traceql.Condition{}, false
}

func operandsEqual(c1, c2 traceql.Condition) bool {
	if len(c1.Operands) != len(c2.Operands) {
		return false
	}

	// todo: sort first?
	for i := 0; i < len(c1.Operands); i++ {
		if !c1.Operands[i].StrictEquals(&c2.Operands[i]) {
			return false
		}
	}

	return true
}
