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
	"math/big"
)

// MergeUnits merges profiles whose sample values may be expressed in
// different but compatible units. Like Merge it returns a new profile
// independent of the input profiles and applies the same merge semantics.
//
// sampleUnits maps every sample type Type of the inputs to the unit it must
// be converted to before merging; all input profiles must contain the same set
// of sample type names (in any order, without duplicates) and sampleUnits
// must contain an entry for each of them, with no extra entries. Sample
// columns of the result are ordered like the first input profile, and each
// column is renamed to the requested unit.
//
// If periodType is non-nil, every input profile must have a period type with
// the same Type name, and the period is converted from the input unit to
// periodType.Unit. If periodType is nil, the period types must be identical,
// as for Merge. NumLabel/NumUnit are not converted.
//
// Supported time units are seconds, milliseconds, microseconds and
// nanoseconds; supported byte units are bytes, kilobytes and megabytes (the
// latter two are decimal, i.e. 1000 and 1000000 bytes). Values in any other
// unit are only accepted when the source and target units are identical.
// Conversions must be exact: conversion to a coarser unit is an error unless
// the value is divisible without remainder, and the result must fit in an
// int64. Integer/rational arithmetic is used throughout; float64 rounding is
// never performed.
//
// All checks and conversions are performed on independent copies, so the
// inputs are left unchanged even when an error is returned.
func MergeUnits(srcs []*Profile, sampleUnits map[string]string, periodType *ValueType) (*Profile, error) {
	if len(srcs) == 0 {
		return nil, fmt.Errorf("no profiles to merge")
	}
	if srcs[0] == nil {
		return nil, fmt.Errorf("profile 0 is nil")
	}

	// Validate the sample type sets and the requested target units.
	wantTypes := make([]string, len(srcs[0].SampleType))
	wantSet := make(map[string]bool, len(srcs[0].SampleType))
	seenInFirst := make(map[string]bool, len(srcs[0].SampleType))
	for i, st := range srcs[0].SampleType {
		if st == nil {
			return nil, fmt.Errorf("profile 0 has a nil sample type")
		}
		if seenInFirst[st.Type] {
			return nil, fmt.Errorf("profile 0 has duplicate sample type %q", st.Type)
		}
		seenInFirst[st.Type] = true
		wantTypes[i] = st.Type
		wantSet[st.Type] = true
		if _, ok := sampleUnits[st.Type]; !ok {
			return nil, fmt.Errorf("no target unit specified for sample type %q", st.Type)
		}
	}
	if len(sampleUnits) != len(wantSet) {
		return nil, fmt.Errorf("target units specify types that are not present in the profiles")
	}

	for idx, src := range srcs {
		if src == nil {
			return nil, fmt.Errorf("profile %d is nil", idx)
		}
		if len(src.SampleType) != len(wantTypes) {
			return nil, fmt.Errorf("profile %d has %d sample types, want %d", idx, len(src.SampleType), len(wantTypes))
		}
		seen := make(map[string]bool, len(wantTypes))
		for _, st := range src.SampleType {
			if st == nil {
				return nil, fmt.Errorf("profile %d has a nil sample type", idx)
			}
			if seen[st.Type] {
				return nil, fmt.Errorf("profile %d has duplicate sample type %q", idx, st.Type)
			}
			seen[st.Type] = true
			if !wantSet[st.Type] {
				return nil, fmt.Errorf("profile %d has sample type %q not present in profile 0", idx, st.Type)
			}
		}

		// Validate the period type.
		switch {
		case periodType != nil:
			if src.PeriodType == nil {
				return nil, fmt.Errorf("profile %d has no period type, want %q", idx, periodType.Type)
			}
			if src.PeriodType.Type != periodType.Type {
				return nil, fmt.Errorf("incompatible period types %q and %q", src.PeriodType.Type, periodType.Type)
			}
		default:
			if (src.PeriodType == nil) != (srcs[0].PeriodType == nil) ||
				(src.PeriodType != nil && !equalValueType(src.PeriodType, srcs[0].PeriodType)) {
				return nil, fmt.Errorf("incompatible period types %v and %v", src.PeriodType, srcs[0].PeriodType)
			}
		}

		// Validate sample value widths (without touching the inputs).
		for si, s := range src.Sample {
			if s == nil {
				return nil, fmt.Errorf("profile %d has a nil sample", idx)
			}
			if len(s.Value) != len(src.SampleType) {
				return nil, fmt.Errorf("profile %d sample %d has %d values vs. %d types", idx, si, len(s.Value), len(src.SampleType))
			}
		}

		// The default sample type must still name a column after reordering.
		if src.DefaultSampleType != "" && !wantSet[src.DefaultSampleType] {
			return nil, fmt.Errorf("profile %d default sample type %q does not match any sample type", idx, src.DefaultSampleType)
		}
	}

	// All validation passed: work on independent copies so a failed
	// conversion can never modify an input profile.
	converted := make([]*Profile, len(srcs))
	for idx, src := range srcs {
		cp := src.Copy()
		if err := convertProfileUnits(cp, idx, wantTypes, sampleUnits, periodType); err != nil {
			return nil, err
		}
		converted[idx] = cp
	}

	return Merge(converted)
}

// convertProfileUnits rewrites cp in place so that its sample columns are
// ordered and expressed according to wantTypes and sampleUnits, and its
// period is expressed in periodType.Unit (when periodType is non-nil).
func convertProfileUnits(cp *Profile, profileIdx int, wantTypes []string, sampleUnits map[string]string, periodType *ValueType) error {
	// Reorder (and convert) sample type columns to match wantTypes.
	srcIndex := make(map[string]int, len(cp.SampleType))
	for i, st := range cp.SampleType {
		srcIndex[st.Type] = i
	}
	newTypes := make([]*ValueType, len(wantTypes))
	remap := make([]int, len(wantTypes))
	for i, typ := range wantTypes {
		j := srcIndex[typ]
		remap[i] = j
		srcType := cp.SampleType[j]
		targetUnit := sampleUnits[typ]
		newTypes[i] = &ValueType{Type: typ, Unit: targetUnit}
		for si, s := range cp.Sample {
			v, err := convertValue(s.Value[j], srcType.Unit, targetUnit)
			if err != nil {
				return fmt.Errorf("profile %d sample %d type %q: %v", profileIdx, si, typ, err)
			}
			s.Value[j] = v
		}
	}

	values := make([]int64, len(wantTypes))
	for _, s := range cp.Sample {
		for i, j := range remap {
			values[i] = s.Value[j]
		}
		copy(s.Value, values)
	}
	cp.SampleType = newTypes

	if periodType != nil {
		period, err := convertValue(cp.Period, cp.PeriodType.Unit, periodType.Unit)
		if err != nil {
			return fmt.Errorf("profile %d period (type %q): %v", profileIdx, periodType.Type, err)
		}
		cp.Period = period
		cp.PeriodType = &ValueType{Type: periodType.Type, Unit: periodType.Unit}
	} else if cp.PeriodType == nil {
		// Merge's header comparison does not tolerate nil period types;
		// use an empty one (all inputs agree on the nil period type).
		cp.PeriodType = &ValueType{}
	}
	return nil
}

// convertValue converts v from fromUnit to toUnit using exact
// integer/rational arithmetic. It never uses float64.
func convertValue(v int64, fromUnit, toUnit string) (int64, error) {
	if fromUnit == toUnit {
		return v, nil
	}
	fromScale, fromFamily, fromKnown := unitScale(fromUnit)
	toScale, toFamily, toKnown := unitScale(toUnit)
	if !fromKnown || !toKnown || fromFamily != toFamily {
		return 0, fmt.Errorf("cannot convert %q to %q", fromUnit, toUnit)
	}

	n := new(big.Int).SetInt64(v)
	n.Mul(n, big.NewInt(fromScale))
	// Divide exactly; the result must be an integer.
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(n, big.NewInt(toScale), rem)
	if rem.Sign() != 0 {
		return 0, fmt.Errorf("value %d %s is not an exact number of %s", v, fromUnit, toUnit)
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("converted value of %d %s overflows int64", v, fromUnit)
	}
	return q.Int64(), nil
}

// unitScale returns the scale of unit relative to the base unit of its
// family, the name of that family and whether the unit is a known supported
// unit.
func unitScale(unit string) (scale int64, family string, ok bool) {
	for _, u := range timeUnits {
		if u.unit == unit {
			return u.scale, "time", true
		}
	}
	for _, u := range byteUnits {
		if u.unit == unit {
			return u.scale, "bytes", true
		}
	}
	return 0, "", false
}

type scaledUnit struct {
	unit  string
	scale int64
}

var timeUnits = []scaledUnit{
	{"seconds", 1e9},
	{"milliseconds", 1e6},
	{"microseconds", 1e3},
	{"nanoseconds", 1},
}

var byteUnits = []scaledUnit{
	{"bytes", 1},
	{"kilobytes", 1e3},
	{"megabytes", 1e6},
}
