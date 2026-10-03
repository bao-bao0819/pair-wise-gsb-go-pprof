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

// TestMergeUnitsSuccess exercises a successful merge across mixed time and
// byte units, including:
//   - integer values above 2^53 where a float64 conversion would be rounded,
//   - negative sample values,
//   - profiles with sample types in a different column order,
//   - conversion of both Period and PeriodType,
//   - preservation of NumLabel/NumUnit,
//   - a Write/Parse roundtrip and hand-computed per-column totals.
func TestMergeUnitsSuccess(t *testing.T) {
	// spaceBytes is deliberately above 2^53 after conversion to kilobytes,
	// and not exactly representable through float64 arithmetic:
	// int64(float64(spaceBytes)/1000) is spaceKB+1.
	const spaceKB int64 = 9_100_000_000_000_001
	const spaceBytes = spaceKB * 1000 // 9_100_000_000_000_001_000 bytes

	locA := &Location{ID: 1, Address: 0x1000}
	locB := &Location{ID: 2, Address: 0x2000}

	p1 := &Profile{
		PeriodType:        &ValueType{Type: "cpu", Unit: "microseconds"},
		Period:            5000,
		DefaultSampleType: "cpu",
		SampleType: []*ValueType{
			{Type: "cpu", Unit: "microseconds"},
			{Type: "space", Unit: "bytes"},
		},
		Sample: []*Sample{
			{
				Location: []*Location{locA},
				Value:    []int64{3, spaceBytes},
				NumLabel: map[string][]int64{"request": {42}},
				NumUnit:  map[string][]string{"request": {"bytes"}},
			},
			{Location: []*Location{locB}, Value: []int64{-7, 2000}},
		},
		Location: []*Location{locA, locB},
	}

	// p2 lists the same sample types but in the opposite order, and uses
	// different source units.
	p2 := &Profile{
		PeriodType:        &ValueType{Type: "cpu", Unit: "milliseconds"},
		Period:            4,
		DefaultSampleType: "space",
		SampleType: []*ValueType{
			{Type: "space", Unit: "kilobytes"},
			{Type: "cpu", Unit: "nanoseconds"},
		},
		Sample: []*Sample{
			{
				Location: []*Location{locA},
				Value:    []int64{5, 2000},
				NumLabel: map[string][]int64{"request": {42}},
				NumUnit:  map[string][]string{"request": {"bytes"}},
			},
			{Location: []*Location{locB}, Value: []int64{3, -1000}},
		},
		Location: []*Location{locA, locB},
	}

	srcs := []*Profile{p1, p2}
	before := make([]string, len(srcs))
	for i, p := range srcs {
		before[i] = p.String()
	}

	targetPeriod := &ValueType{Type: "cpu", Unit: "nanoseconds"}
	merged, err := MergeUnits(srcs, map[string]string{
		"cpu":   "nanoseconds",
		"space": "kilobytes",
	}, targetPeriod)
	if err != nil {
		t.Fatalf("MergeUnits() failed: %v", err)
	}

	if err := merged.CheckValid(); err != nil {
		t.Fatalf("merged profile is invalid: %v", err)
	}

	// Inputs must not have been modified.
	for i, p := range srcs {
		if got := p.String(); got != before[i] {
			t.Errorf("input profile %d was modified:\n--- before ---\n%s\n--- after ---\n%s", i, before[i], got)
		}
	}

	// Columns follow the first input profile's order and carry target units.
	wantTypes := []*ValueType{
		{Type: "cpu", Unit: "nanoseconds"},
		{Type: "space", Unit: "kilobytes"},
	}
	if len(merged.SampleType) != len(wantTypes) {
		t.Fatalf("got %d sample types, want %d", len(merged.SampleType), len(wantTypes))
	}
	for i, want := range wantTypes {
		if !equalValueType(merged.SampleType[i], want) {
			t.Errorf("sample type %d = %v, want %v", i, merged.SampleType[i], want)
		}
	}

	// p1 period 5000us = 5,000,000ns; p2 period 4ms = 4,000,000ns; Merge
	// keeps the maximum.
	if merged.Period != 5_000_000 {
		t.Errorf("Period = %d, want 5000000", merged.Period)
	}
	if !equalValueType(merged.PeriodType, targetPeriod) {
		t.Errorf("PeriodType = %v, want %v", merged.PeriodType, targetPeriod)
	}

	// Hand-computed merged samples (p2 columns were reordered):
	//   locA: cpu = 3us + 2000ns = 5000ns; space = spaceBytes/1000 + 5kb
	//       = spaceKB + 5
	//   locB: cpu = -7us + -1000ns = -8000ns; space = 2000b/1000 + 3kb = 5kb
	wantByAddr := map[uint64][]int64{
		locA.Address: {5000, spaceKB + 5},
		locB.Address: {-8000, 5},
	}
	byAddr := make(map[uint64]*Sample, len(merged.Sample))
	for _, s := range merged.Sample {
		if len(s.Location) != 1 {
			t.Fatalf("sample has %d locations, want 1", len(s.Location))
		}
		byAddr[s.Location[0].Address] = s
	}
	for addr, want := range wantByAddr {
		s, ok := byAddr[addr]
		if !ok {
			t.Errorf("missing sample at address %#x", addr)
			continue
		}
		if s.Value[0] != want[0] || s.Value[1] != want[1] {
			t.Errorf("sample at %#x = %v, want %v", addr, s.Value, want)
		}
	}

	// Per-column totals, hand-computed from the converted inputs:
	//   cpu   = 5000 + -8000 = -3000 ns
	//   space = (spaceKB + 5) + 5 = spaceKB + 10 kb
	totals := make([]int64, len(wantTypes))
	for _, s := range merged.Sample {
		for i, v := range s.Value {
			totals[i] += v
		}
	}
	wantTotals := []int64{-3000, spaceKB + 10}
	for i := range wantTotals {
		if totals[i] != wantTotals[i] {
			t.Errorf("column %q total = %d, want %d", wantTypes[i].Type, totals[i], wantTotals[i])
		}
	}

	// NumLabel/NumUnit are out of scope for conversion but must survive.
	gotNum := byAddr[locA.Address].NumLabel["request"]
	gotUnit := byAddr[locA.Address].NumUnit["request"]
	if len(gotNum) != 1 || gotNum[0] != 42 || len(gotUnit) != 1 || gotUnit[0] != "bytes" {
		t.Errorf("numeric label not preserved: %v %v", byAddr[locA.Address].NumLabel, byAddr[locA.Address].NumUnit)
	}

	// Write/Parse roundtrip.
	var buf bytes.Buffer
	if err := merged.Write(&buf); err != nil {
		t.Fatalf("Write() failed: %v", err)
	}
	round, err := Parse(&buf)
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	if err := round.CheckValid(); err != nil {
		t.Fatalf("roundtripped profile is invalid: %v", err)
	}
	roundTotals := make([]int64, len(merged.SampleType))
	for _, s := range round.Sample {
		for i, v := range s.Value {
			roundTotals[i] += v
		}
	}
	for i := range wantTotals {
		if roundTotals[i] != wantTotals[i] {
			t.Errorf("after roundtrip column %q total = %d, want %d", wantTypes[i].Type, roundTotals[i], wantTotals[i])
		}
	}
	if round.Period != merged.Period || !equalValueType(round.PeriodType, merged.PeriodType) {
		t.Errorf("after roundtrip period = %d %v, want %d %v", round.Period, round.PeriodType, merged.Period, merged.PeriodType)
	}
}

func TestMergeUnitsErrors(t *testing.T) {
	loc := &Location{ID: 1, Address: 0x1000}

	// Profile whose only sample value cannot be converted to a coarser unit.
	indivisible := func() *Profile {
		return &Profile{
			PeriodType: &ValueType{Type: "cpu", Unit: "bytes"},
			Period:     1,
			SampleType: []*ValueType{{Type: "space", Unit: "bytes"}},
			Sample:     []*Sample{{Location: []*Location{loc}, Value: []int64{7}}},
			Location:   []*Location{loc},
		}
	}

	// Profile whose period cannot be converted to the target period unit.
	badPeriod := func() *Profile {
		return &Profile{
			PeriodType: &ValueType{Type: "cpu", Unit: "microseconds"},
			Period:     7,
			SampleType: []*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
			Sample:     []*Sample{{Location: []*Location{loc}, Value: []int64{1000}}},
			Location:   []*Location{loc},
		}
	}

	profile := func(types []*ValueType, values []int64) *Profile {
		return &Profile{
			PeriodType: &ValueType{Type: "cpu", Unit: "nanoseconds"},
			Period:     1,
			SampleType: types,
			Sample:     []*Sample{{Location: []*Location{loc}, Value: values}},
			Location:   []*Location{loc},
		}
	}

	cpuNS := &ValueType{Type: "cpu", Unit: "nanoseconds"}
	spaceB := &ValueType{Type: "space", Unit: "bytes"}

	for _, tc := range []struct {
		desc        string
		srcs        []*Profile
		sampleUnits map[string]string
		periodType  *ValueType
		wantErr     string
	}{
		{
			desc:        "no profiles",
			srcs:        nil,
			sampleUnits: map[string]string{},
			wantErr:     "no profiles",
		},
		{
			desc:        "nil profile",
			srcs:        []*Profile{nil},
			sampleUnits: map[string]string{},
			wantErr:     "profile 0 is nil",
		},
		{
			desc: "duplicate sample type name in first profile",
			srcs: []*Profile{profile([]*ValueType{cpuNS, {Type: "cpu", Unit: "microseconds"}}, []int64{1, 2})},
			sampleUnits: map[string]string{
				"cpu": "nanoseconds",
			},
			wantErr: "duplicate sample type",
		},
		{
			desc: "duplicate sample type name in second profile",
			srcs: []*Profile{
				profile([]*ValueType{cpuNS, spaceB}, []int64{1, 2}),
				profile([]*ValueType{cpuNS, {Type: "cpu", Unit: "microseconds"}}, []int64{1, 2}),
			},
			sampleUnits: map[string]string{
				"cpu":   "nanoseconds",
				"space": "bytes",
			},
			wantErr: "duplicate sample type",
		},
		{
			desc: "sample type sets differ",
			srcs: []*Profile{
				profile([]*ValueType{cpuNS}, []int64{1}),
				profile([]*ValueType{{Type: "space", Unit: "bytes"}}, []int64{1}),
			},
			sampleUnits: map[string]string{
				"cpu": "nanoseconds",
			},
			wantErr: "not present",
		},
		{
			desc: "missing target unit",
			srcs: []*Profile{
				profile([]*ValueType{cpuNS, spaceB}, []int64{1, 2}),
			},
			sampleUnits: map[string]string{
				"cpu": "nanoseconds",
			},
			wantErr: "no target unit specified",
		},
		{
			desc: "extra target unit",
			srcs: []*Profile{
				profile([]*ValueType{cpuNS}, []int64{1}),
			},
			sampleUnits: map[string]string{
				"cpu":   "nanoseconds",
				"space": "bytes",
			},
			wantErr: "not present in the profiles",
		},
		{
			desc: "cannot convert to coarser unit",
			srcs: []*Profile{indivisible()},
			sampleUnits: map[string]string{
				"space": "kilobytes",
			},
			wantErr: "not an exact number",
		},
		{
			desc: "converted value overflows int64",
			srcs: []*Profile{
				profile([]*ValueType{{Type: "space", Unit: "megabytes"}}, []int64{9_300_000_000_000_000}),
			},
			sampleUnits: map[string]string{
				"space": "kilobytes",
			},
			wantErr: "overflows int64",
		},
		{
			desc: "incompatible unit families",
			srcs: []*Profile{
				profile([]*ValueType{{Type: "cpu", Unit: "nanoseconds"}}, []int64{1}),
			},
			sampleUnits: map[string]string{
				"cpu": "bytes",
			},
			wantErr: "cannot convert",
		},
		{
			desc: "unknown unit can only match itself",
			srcs: []*Profile{
				profile([]*ValueType{{Type: "cpu", Unit: "cycles"}}, []int64{1}),
			},
			sampleUnits: map[string]string{
				"cpu": "nanoseconds",
			},
			wantErr: "cannot convert",
		},
		{
			desc: "period conversion not exact",
			srcs: []*Profile{badPeriod()},
			sampleUnits: map[string]string{
				"cpu": "nanoseconds",
			},
			periodType: &ValueType{Type: "cpu", Unit: "milliseconds"},
			wantErr:    "period",
		},
		{
			desc: "period type names differ",
			srcs: []*Profile{
				{
					PeriodType: &ValueType{Type: "wall", Unit: "nanoseconds"},
					SampleType: []*ValueType{cpuNS},
					Sample:     []*Sample{{Location: []*Location{loc}, Value: []int64{1}}},
					Location:   []*Location{loc},
				},
			},
			sampleUnits: map[string]string{
				"cpu": "nanoseconds",
			},
			periodType: &ValueType{Type: "cpu", Unit: "nanoseconds"},
			wantErr:    "incompatible period types",
		},
		{
			desc: "missing target period type",
			srcs: []*Profile{
				{
					SampleType: []*ValueType{cpuNS},
					Sample:     []*Sample{{Location: []*Location{loc}, Value: []int64{1}}},
					Location:   []*Location{loc},
				},
			},
			sampleUnits: map[string]string{
				"cpu": "nanoseconds",
			},
			periodType: &ValueType{Type: "cpu", Unit: "nanoseconds"},
			wantErr:    "has no period type",
		},
		{
			desc: "default sample type points to a missing column",
			srcs: []*Profile{
				{
					PeriodType:        &ValueType{Type: "cpu", Unit: "nanoseconds"},
					DefaultSampleType: "space",
					SampleType:        []*ValueType{cpuNS},
					Sample:            []*Sample{{Location: []*Location{loc}, Value: []int64{1}}},
					Location:          []*Location{loc},
				},
			},
			sampleUnits: map[string]string{
				"cpu": "nanoseconds",
			},
			wantErr: "default sample type",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			before := make([]string, len(tc.srcs))
			for i, p := range tc.srcs {
				if p != nil {
					before[i] = p.String()
				}
			}
			_, err := MergeUnits(tc.srcs, tc.sampleUnits, tc.periodType)
			if err == nil {
				t.Fatalf("MergeUnits() succeeded, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("MergeUnits() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
			for i, p := range tc.srcs {
				if p == nil {
					continue
				}
				if got := p.String(); got != before[i] {
					t.Errorf("input profile %d modified after failure:\n--- before ---\n%s\n--- after ---\n%s", i, before[i], got)
				}
			}
		})
	}
}

// TestMergeUnitsUnknownUnitPassThrough verifies that unknown units are
// accepted when source and target units are identical, and that values then
// pass through unchanged.
func TestMergeUnitsUnknownUnitPassThrough(t *testing.T) {
	loc := &Location{ID: 1, Address: 0x1000}
	mk := func(unit string, value int64) *Profile {
		return &Profile{
			PeriodType: &ValueType{Type: "cpu", Unit: "cycles"},
			Period:     3,
			SampleType: []*ValueType{{Type: "cpu", Unit: unit}},
			Sample:     []*Sample{{Location: []*Location{loc}, Value: []int64{value}}},
			Location:   []*Location{loc},
		}
	}
	srcs := []*Profile{mk("cycles", 5), mk("cycles", 7)}
	merged, err := MergeUnits(srcs, map[string]string{"cpu": "cycles"}, nil)
	if err != nil {
		t.Fatalf("MergeUnits() failed: %v", err)
	}
	if got := merged.Sample[0].Value[0]; got != 12 {
		t.Errorf("merged value = %d, want 12", got)
	}
	if got := merged.SampleType[0].Unit; got != "cycles" {
		t.Errorf("unit = %q, want %q", got, "cycles")
	}
	if got := merged.Period; got != 3 {
		t.Errorf("Period = %d, want 3", got)
	}
}
