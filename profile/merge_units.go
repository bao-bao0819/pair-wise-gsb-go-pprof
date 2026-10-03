// Copyright 2026 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package profile

import (
	"fmt"
	"math"
	"math/big"
)

// MergeWithUnits merges the profiles in srcs into a single Profile after
// converting their sample values to explicit target units.
//
// targetUnits gives the destination unit for each SampleType.Type. Every
// input profile must contain exactly the same set of sample type names
// (their order and their original units may differ), and duplicate sample
// type names within a profile are rejected. The merged profile's sample
// types use the order of the first profile and the units requested in
// targetUnits.
//
// Values are converted with exact integer arithmetic: converting to finer
// units multiplies, while converting to coarser units is only allowed when
// the value is exactly divisible. float64 rounding is never used, and a
// result that would not fit in int64 is rejected. Recognized time units are
// seconds, milliseconds, microseconds and nanoseconds; recognized byte
// units are bytes, kilobytes and megabytes (multiples of 1000). Conversions
// between unrecognized units are only allowed when the units are identical.
//
// PeriodType/Period are converted with the same rules, using the target
// unit requested for the period type's name. NumLabel/NumUnit are not
// converted.
//
// The inputs are never modified: each profile is converted on an
// independent copy and merged with the semantics of Merge.
func MergeWithUnits(srcs []*Profile, targetUnits map[string]string) (*Profile, error) {
	if len(srcs) == 0 {
		return nil, fmt.Errorf("no profiles to merge")
	}

	// The canonical sample type order is taken from the first profile.
	canonical := make([]string, 0, len(srcs[0].SampleType))
	seenTypes := make(map[string]bool, len(srcs[0].SampleType))
	for _, st := range srcs[0].SampleType {
		if seenTypes[st.Type] {
			return nil, fmt.Errorf("input #%d has duplicate sample type %q", 0, st.Type)
		}
		seenTypes[st.Type] = true
		canonical = append(canonical, st.Type)
	}

	// Resolve and validate the requested conversion once per profile,
	// without touching the inputs. Each entry maps a canonical sample type
	// to the multiply/divide factors for converting that profile's column.
	type columnConv struct {
		index    int // column index in the source profile
		multiply int64
		divide   int64
	}
	convs := make([][]columnConv, len(srcs))
	for pi, src := range srcs {
		columnByName := make(map[string]int, len(src.SampleType))
		for i, st := range src.SampleType {
			if _, ok := columnByName[st.Type]; ok {
				return nil, fmt.Errorf("input #%d has duplicate sample type %q", pi, st.Type)
			}
			columnByName[st.Type] = i
		}
		if len(columnByName) != len(canonical) {
			return nil, fmt.Errorf("input #%d has %d sample types, want %d (matching %v)", pi, len(columnByName), len(canonical), canonical)
		}

		profileConvs := make([]columnConv, len(canonical))
		for ci, name := range canonical {
			idx, ok := columnByName[name]
			if !ok {
				return nil, fmt.Errorf("input #%d is missing sample type %q", pi, name)
			}
			targetUnit, ok := targetUnits[name]
			if !ok {
				return nil, fmt.Errorf("no target unit specified for sample type %q", name)
			}
			multiply, divide, err := resolveUnitConversion(src.SampleType[idx].Unit, targetUnit)
			if err != nil {
				return nil, fmt.Errorf("input #%d sample type %q: %v", pi, name, err)
			}
			profileConvs[ci] = columnConv{index: idx, multiply: multiply, divide: divide}
		}
		convs[pi] = profileConvs

		// Validate every sample value before anything is copied.
		for si, s := range src.Sample {
			if len(s.Value) != len(src.SampleType) {
				return nil, fmt.Errorf("input #%d sample #%d has %d values vs. %d types", pi, si, len(s.Value), len(src.SampleType))
			}
			for ci, conv := range profileConvs {
				if _, err := convertUnitValue(s.Value[conv.index], conv.multiply, conv.divide); err != nil {
					return nil, fmt.Errorf("input #%d sample #%d type %q value %d: %v", pi, si, canonical[ci], s.Value[conv.index], err)
				}
			}
		}

		if d := src.DefaultSampleType; d != "" {
			if _, ok := columnByName[d]; !ok {
				return nil, fmt.Errorf("input #%d DefaultSampleType %q does not name a sample type", pi, d)
			}
		}
	}

	type unitConv struct {
		multiply int64
		divide   int64
	}

	// Validate the period conversion up front, remembering the factors for
	// each input profile.
	periodType := srcs[0].PeriodType
	var periodConvs []unitConv
	var periodTargetUnit string
	if periodType != nil {
		for _, src := range srcs {
			if src.PeriodType == nil || src.PeriodType.Type != periodType.Type {
				return nil, fmt.Errorf("incompatible period types %v and %v", src.PeriodType, periodType)
			}
		}
		if target, ok := targetUnits[periodType.Type]; ok {
			periodTargetUnit = target
		} else {
			// No requested conversion: all periods must already share a unit.
			periodTargetUnit = periodType.Unit
		}
		periodConvs = make([]unitConv, len(srcs))
		for pi, src := range srcs {
			multiply, divide, err := resolveUnitConversion(src.PeriodType.Unit, periodTargetUnit)
			if err != nil {
				return nil, fmt.Errorf("input #%d period type %q: %v", pi, periodType.Type, err)
			}
			if _, err := convertUnitValue(src.Period, multiply, divide); err != nil {
				return nil, fmt.Errorf("input #%d period %d: %v", pi, src.Period, err)
			}
			periodConvs[pi] = unitConv{multiply, divide}
		}
	}

	// All inputs are valid. Convert independent copies, then merge.
	converted := make([]*Profile, len(srcs))
	for pi, src := range srcs {
		cp := src.Copy()

		types := make([]*ValueType, len(canonical))
		for ci, name := range canonical {
			types[ci] = &ValueType{Type: name, Unit: targetUnits[name]}
		}
		cp.SampleType = types

		for _, s := range cp.Sample {
			values := make([]int64, len(canonical))
			for ci, conv := range convs[pi] {
				v, err := convertUnitValue(s.Value[conv.index], conv.multiply, conv.divide)
				if err != nil {
					// Already validated above; a failure here would be a bug.
					return nil, fmt.Errorf("input #%d sample type %q: %v", pi, canonical[ci], err)
				}
				values[ci] = v
			}
			s.Value = values
		}

		if periodType != nil {
			cp.PeriodType = &ValueType{Type: periodType.Type, Unit: periodTargetUnit}
			period, err := convertUnitValue(cp.Period, periodConvs[pi].multiply, periodConvs[pi].divide)
			if err != nil {
				return nil, fmt.Errorf("input #%d period: %v", pi, err)
			}
			cp.Period = period
		}

		converted[pi] = cp
	}

	return Merge(converted)
}

// unitInfo describes a recognized convertible unit as a multiple of the
// base unit of its family.
type unitInfo struct {
	family string
	scale  int64
}

var knownUnits = map[string]unitInfo{
	"seconds":      {"time", 1_000_000_000},
	"milliseconds": {"time", 1_000_000},
	"microseconds": {"time", 1_000},
	"nanoseconds":  {"time", 1},
	"bytes":        {"bytes", 1},
	"kilobytes":    {"bytes", 1_000},
	"megabytes":    {"bytes", 1_000_000},
}

// resolveUnitConversion returns the exact factors converting a value
// expressed in fromUnit to targetUnit: result = value * multiply / divide.
// Equal units, including two identical unrecognized units, produce an
// identity (multiply/divide == 1) conversion.
func resolveUnitConversion(fromUnit, targetUnit string) (multiply, divide int64, err error) {
	if fromUnit == targetUnit {
		return 1, 1, nil
	}
	from, okFrom := knownUnits[fromUnit]
	to, okTo := knownUnits[targetUnit]
	if !okFrom || !okTo {
		return 0, 0, fmt.Errorf("cannot convert unit %q to %q", fromUnit, targetUnit)
	}
	if from.family != to.family {
		return 0, 0, fmt.Errorf("cannot convert %s unit %q to %s unit %q", from.family, fromUnit, to.family, targetUnit)
	}
	if from.scale >= to.scale {
		return from.scale / to.scale, 1, nil
	}
	return 1, to.scale / from.scale, nil
}

// convertUnitValue computes value * multiply / divide using exact integer
// arithmetic. It fails when the division is not exact or when the result
// does not fit in int64.
func convertUnitValue(value, multiply, divide int64) (int64, error) {
	v := big.NewInt(value)
	v.Mul(v, big.NewInt(multiply))
	var r big.Int
	v.QuoRem(v, big.NewInt(divide), &r)
	if r.Sign() != 0 {
		return 0, fmt.Errorf("not exactly convertible: %d leaves a remainder when divided by %d", value, divide)
	}
	if v.Cmp(big.NewInt(math.MaxInt64)) > 0 || v.Cmp(big.NewInt(math.MinInt64)) < 0 {
		return 0, fmt.Errorf("converted value %d overflows int64", v)
	}
	return v.Int64(), nil
}
