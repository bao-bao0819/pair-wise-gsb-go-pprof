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
	"bytes"
	"strings"
	"testing"
)

// bigCPUMicros is above 2^53 and odd, so it is not representable as a
// float64: float64(bigCPUMicros)*1000 is rounded away from the exact
// nanosecond result bigCPUMicros*1000.
const bigCPUMicros = int64(1)<<53 + 9 // 9007199254741001 microseconds

func unitsTestProfile(t *testing.T, sampleTypes []*ValueType, values [][]int64, periodType *ValueType, period int64) *Profile {
	t.Helper()
	var locations []*Location
	var samples []*Sample
	for _, v := range values {
		loc := &Location{
			ID:      uint64(len(locations) + 1),
			Address: uint64(len(locations) + 1),
		}
		locations = append(locations, loc)
		samples = append(samples, &Sample{
			Location: []*Location{loc},
			Value:    append([]int64(nil), v...),
		})
	}
	p := &Profile{
		SampleType:        sampleTypes,
		DefaultSampleType: "cpu",
		Sample:            samples,
		Location:          locations,
		PeriodType:        periodType,
		Period:            period,
	}
	p.DefaultSampleType = sampleTypes[0].Type
	return p
}

func TestMergeWithUnits(t *testing.T) {
	// Profile 0: columns ordered cpu (microseconds), space (bytes), samples
	// (count, an unrecognized unit). Includes a negative sample value and a
	// value over 2^53 whose conversion to nanoseconds defeats float64.
	p0 := unitsTestProfile(t,
		[]*ValueType{
			{Type: "cpu", Unit: "microseconds"},
			{Type: "space", Unit: "bytes"},
			{Type: "samples", Unit: "count"},
		},
		[][]int64{
			{bigCPUMicros, 2_000_000, 3},
			{-7, 4_000_000, -1},
		},
		&ValueType{Type: "cpu", Unit: "microseconds"}, 2)

	// Profile 1: same type names in a different order with mixed units.
	p1 := unitsTestProfile(t,
		[]*ValueType{
			{Type: "space", Unit: "kilobytes"},
			{Type: "samples", Unit: "count"},
			{Type: "cpu", Unit: "nanoseconds"},
		},
		[][]int64{
			{7, 5, 3000},
		},
		&ValueType{Type: "cpu", Unit: "nanoseconds"}, 3000)

	before0 := p0.String()
	before1 := p1.String()

	targetUnits := map[string]string{
		"cpu":     "nanoseconds",
		"space":   "bytes",
		"samples": "count",
	}
	merged, err := MergeWithUnits([]*Profile{p0, p1}, targetUnits)
	if err != nil {
		t.Fatalf("MergeWithUnits failed: %v", err)
	}

	if got := p0.String(); got != before0 {
		t.Errorf("input #0 was modified:\n%s", got)
	}
	if got := p1.String(); got != before1 {
		t.Errorf("input #1 was modified:\n%s", got)
	}

	if err := merged.CheckValid(); err != nil {
		t.Fatalf("merged profile is invalid: %v", err)
	}

	// Column order follows the first profile; units are the target units.
	wantTypes := []*ValueType{
		{Type: "cpu", Unit: "nanoseconds"},
		{Type: "space", Unit: "bytes"},
		{Type: "samples", Unit: "count"},
	}
	if len(merged.SampleType) != len(wantTypes) {
		t.Fatalf("got %d sample types, want %d", len(merged.SampleType), len(wantTypes))
	}
	for i, want := range wantTypes {
		if *merged.SampleType[i] != *want {
			t.Errorf("sample type #%d = %v, want %v", i, *merged.SampleType[i], *want)
		}
	}

	totals := make([]int64, len(wantTypes))
	for _, s := range merged.Sample {
		for i, v := range s.Value {
			totals[i] += v
		}
	}
	// cpu: bigCPUMicros*1000 - 7*1000 + 3000
	wantCPU := bigCPUMicros*1000 - 7000 + 3000
	// space: 2,000,000 + 4,000,000 + 7 kilobytes
	wantSpace := int64(2_000_000 + 4_000_000 + 7*1000)
	wantSamples := int64(3 - 1 + 5)
	wantTotals := []int64{wantCPU, wantSpace, wantSamples}
	for i, want := range wantTotals {
		if totals[i] != want {
			t.Errorf("column %q total = %d, want %d", wantTypes[i].Type, totals[i], want)
		}
	}

	if merged.PeriodType == nil || merged.PeriodType.Type != "cpu" || merged.PeriodType.Unit != "nanoseconds" {
		t.Errorf("period type = %v, want cpu/nanoseconds", merged.PeriodType)
	}
	// max(2 us, 3000 ns) = max(2000 ns, 3000 ns) = 3000 ns.
	if merged.Period != 3000 {
		t.Errorf("period = %d, want 3000", merged.Period)
	}
	if merged.DefaultSampleType != "cpu" {
		t.Errorf("default sample type = %q, want cpu", merged.DefaultSampleType)
	}

	var buf bytes.Buffer
	if err := merged.Write(&buf); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	roundtrip, err := Parse(&buf)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if err := roundtrip.CheckValid(); err != nil {
		t.Fatalf("roundtripped profile is invalid: %v", err)
	}
	if got := roundtrip.String(); got != merged.String() {
		t.Errorf("roundtrip mismatch (-want,+got):\n%s", got)
	}
}

func TestMergeWithUnitsErrors(t *testing.T) {
	base := func(sampleTypes []*ValueType, values ...[]int64) *Profile {
		return unitsTestProfile(t, sampleTypes, values, nil, 0)
	}

	for _, tc := range []struct {
		desc        string
		profs       []*Profile
		targetUnits map[string]string
		errContains string
	}{
		{
			desc: "duplicate sample type name in an input",
			profs: []*Profile{
				base([]*ValueType{
					{Type: "cpu", Unit: "nanoseconds"},
					{Type: "cpu", Unit: "microseconds"},
				}, []int64{1, 2}),
			},
			targetUnits: map[string]string{"cpu": "nanoseconds"},
			errContains: "duplicate sample type",
		},
		{
			desc: "mismatched sample type name sets",
			profs: []*Profile{
				base([]*ValueType{{Type: "cpu", Unit: "nanoseconds"}}, []int64{1}),
				base([]*ValueType{{Type: "wall", Unit: "nanoseconds"}}, []int64{1}),
			},
			targetUnits: map[string]string{"cpu": "nanoseconds"},
			errContains: "missing sample type",
		},
		{
			desc: "coarser unit not exactly divisible",
			profs: []*Profile{
				base([]*ValueType{{Type: "cpu", Unit: "nanoseconds"}}, []int64{999}),
			},
			targetUnits: map[string]string{"cpu": "microseconds"},
			errContains: "sample #0",
		},
		{
			desc: "negative value not exactly divisible",
			profs: []*Profile{
				base([]*ValueType{{Type: "cpu", Unit: "nanoseconds"}}, []int64{-999}),
			},
			targetUnits: map[string]string{"cpu": "microseconds"},
			errContains: "sample #0",
		},
		{
			desc: "converted value overflows int64",
			profs: []*Profile{
				base([]*ValueType{{Type: "space", Unit: "megabytes"}}, []int64{1 << 50}),
			},
			targetUnits: map[string]string{"space": "bytes"},
			errContains: "overflows int64",
		},
		{
			desc: "unknown units differ",
			profs: []*Profile{
				base([]*ValueType{{Type: "cpu", Unit: "cycles"}}, []int64{1}),
				base([]*ValueType{{Type: "cpu", Unit: "ticks"}}, []int64{1}),
			},
			targetUnits: map[string]string{"cpu": "cycles"},
			errContains: "cannot convert",
		},
		{
			desc: "conversion across time and byte families",
			profs: []*Profile{
				base([]*ValueType{{Type: "space", Unit: "seconds"}}, []int64{1}),
			},
			targetUnits: map[string]string{"space": "bytes"},
			errContains: "cannot convert",
		},
		{
			desc: "missing target unit",
			profs: []*Profile{
				base([]*ValueType{{Type: "cpu", Unit: "nanoseconds"}}, []int64{1}),
			},
			targetUnits: map[string]string{},
			errContains: "no target unit",
		},
		{
			desc: "DefaultSampleType points at missing column",
			profs: func() []*Profile {
				p := unitsTestProfile(t,
					[]*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
					[][]int64{{1}}, nil, 0)
				p.DefaultSampleType = "gone"
				return []*Profile{p}
			}(),
			targetUnits: map[string]string{"cpu": "nanoseconds"},
			errContains: "DefaultSampleType",
		},
		{
			desc: "period not exactly convertible",
			profs: []*Profile{
				unitsTestProfile(t,
					[]*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
					[][]int64{{1000}},
					&ValueType{Type: "cpu", Unit: "nanoseconds"}, 5),
			},
			targetUnits: map[string]string{"cpu": "microseconds"},
			errContains: "period",
		},
		{
			desc: "incompatible period types",
			profs: []*Profile{
				unitsTestProfile(t,
					[]*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
					[][]int64{{1000}},
					&ValueType{Type: "cpu", Unit: "nanoseconds"}, 1000),
				unitsTestProfile(t,
					[]*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
					[][]int64{{1000}},
					&ValueType{Type: "wall", Unit: "nanoseconds"}, 1000),
			},
			targetUnits: map[string]string{"cpu": "nanoseconds", "wall": "nanoseconds"},
			errContains: "incompatible period types",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			before := make([]string, len(tc.profs))
			for i, p := range tc.profs {
				before[i] = p.String()
			}
			_, err := MergeWithUnits(tc.profs, tc.targetUnits)
			if err == nil {
				t.Fatalf("MergeWithUnits succeeded, want error containing %q", tc.errContains)
			}
			if !strings.Contains(err.Error(), tc.errContains) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.errContains)
			}
			for i, p := range tc.profs {
				if got := p.String(); got != before[i] {
					t.Errorf("input #%d was modified after failure:\n%s", i, got)
				}
			}
		})
	}
}

func TestMergeWithUnitsRejectsMismatchedUnitsLikeMerge(t *testing.T) {
	p0 := unitsTestProfile(t,
		[]*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		[][]int64{{1000}},
		&ValueType{Type: "cpu", Unit: "nanoseconds"}, 1000)
	p1 := unitsTestProfile(t,
		[]*ValueType{{Type: "cpu", Unit: "microseconds"}},
		[][]int64{{1}},
		&ValueType{Type: "cpu", Unit: "microseconds"}, 1)
	if _, err := Merge([]*Profile{p0, p1}); err == nil {
		t.Error("plain Merge accepted profiles with different units")
	}
}
