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
	"math"
	"testing"
)

// relocationTestProfile returns a profile with two mappings, locations
// shared between samples, and a location with multiple inlined functions.
func relocationTestProfile() *Profile {
	f1 := &Function{ID: 1, Name: "main", SystemName: "main", Filename: "main.c", StartLine: 1}
	f2 := &Function{ID: 2, Name: "inlined1", SystemName: "inlined1", Filename: "inlined.c", StartLine: 5}
	f3 := &Function{ID: 3, Name: "inlined2", SystemName: "inlined2", Filename: "inlined.c", StartLine: 9}

	m1 := &Mapping{
		ID:              1,
		Start:           0x1000,
		Limit:           0x3000,
		Offset:          0x100,
		File:            "/bin/prog",
		BuildID:         "build1",
		HasFunctions:    true,
		HasFilenames:    true,
		HasLineNumbers:  true,
		HasInlineFrames: true,
	}
	m2 := &Mapping{
		ID:           2,
		Start:        0x8000,
		Limit:        0x9000,
		Offset:       0,
		File:         "/lib/libc.so",
		BuildID:      "build2",
		HasFunctions: true,
	}

	l1 := &Location{ID: 1, Mapping: m1, Address: 0x1800, Line: []Line{{Function: f1, Line: 10}}}
	l2 := &Location{ID: 2, Mapping: m1, Address: 0x2000, Line: []Line{
		{Function: f2, Line: 20, Column: 3},
		{Function: f3, Line: 30},
	}}
	l3 := &Location{ID: 3, Mapping: m1, Address: 0x2fff, Line: []Line{{Function: f1, Line: 11}}}
	l4 := &Location{ID: 4, Mapping: m2, Address: 0x8400, Line: []Line{{Function: f1, Line: 12}}}

	return &Profile{
		SampleType: []*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample: []*Sample{
			{Value: []int64{1}, Location: []*Location{l1, l2}},
			{Value: []int64{2}, Location: []*Location{l1}},
			{Value: []int64{3}, Location: []*Location{l3, l4}},
		},
		Mapping:    []*Mapping{m1, m2},
		Location:   []*Location{l1, l2, l3, l4},
		Function:   []*Function{f1, f2, f3},
		PeriodType: &ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:     1,
	}
}

// fileOffset returns the file-relative address of a location.
func fileOffset(l *Location) uint64 {
	return l.Address - l.Mapping.Start + l.Mapping.Offset
}

func TestRelocateMappings(t *testing.T) {
	p := relocationTestProfile()
	orig := p.String()

	// Split mapping 1 into two segments and swap their order in the
	// target address space.
	plan := map[uint64][]MappingSegment{
		1: {
			{Start: 0x1000, Limit: 0x2000, NewStart: 0x5000},
			{Start: 0x2000, Limit: 0x3000, NewStart: 0x4000},
		},
	}
	rp, err := p.RelocateMappings(plan)
	if err != nil {
		t.Fatalf("RelocateMappings: %v", err)
	}

	// The original profile must be unmodified.
	if got := p.String(); got != orig {
		t.Errorf("original profile modified, got:\n%s\nwant:\n%s", got, orig)
	}

	// Check the new mappings: the first segment keeps ID 1, the second
	// gets a fresh ID, mapping 2 is untouched.
	if len(rp.Mapping) != 3 {
		t.Fatalf("got %d mappings, want 3", len(rp.Mapping))
	}
	type wantMapping struct {
		id           uint64
		start, limit uint64
		offset       uint64
		file         string
	}
	wantMappings := []wantMapping{
		{id: 1, start: 0x5000, limit: 0x6000, offset: 0x100, file: "/bin/prog"},
		{id: 3, start: 0x4000, limit: 0x5000, offset: 0x1100, file: "/bin/prog"},
		{id: 2, start: 0x8000, limit: 0x9000, offset: 0, file: "/lib/libc.so"},
	}
	for i, want := range wantMappings {
		m := rp.Mapping[i]
		if m.ID != want.id || m.Start != want.start || m.Limit != want.limit || m.Offset != want.offset || m.File != want.file {
			t.Errorf("mapping %d: got %d: %#x/%#x/%#x %s, want %d: %#x/%#x/%#x %s",
				i, m.ID, m.Start, m.Limit, m.Offset, m.File,
				want.id, want.start, want.limit, want.offset, want.file)
		}
	}
	seg1, seg2 := rp.Mapping[0], rp.Mapping[1]
	if seg1.BuildID != "build1" || !seg1.HasFunctions || !seg1.HasFilenames || !seg1.HasLineNumbers || !seg1.HasInlineFrames {
		t.Errorf("segment 1 lost mapping attributes: %+v", seg1)
	}
	if seg2.BuildID != "build1" || !seg2.HasFunctions || !seg2.HasFilenames || !seg2.HasLineNumbers || !seg2.HasInlineFrames {
		t.Errorf("segment 2 lost mapping attributes: %+v", seg2)
	}

	// Check relocated locations. The boundary address 0x2000 must belong
	// to the second segment only.
	wantAddrs := []struct {
		id      uint64
		addr    uint64
		mapping *Mapping
	}{
		{id: 1, addr: 0x5800, mapping: seg1},
		{id: 2, addr: 0x4000, mapping: seg2},
		{id: 3, addr: 0x4fff, mapping: seg2},
		{id: 4, addr: 0x8400, mapping: rp.Mapping[2]},
	}
	origLocations := p.Location
	for i, want := range wantAddrs {
		l := rp.Location[i]
		if l.ID != want.id || l.Address != want.addr || l.Mapping != want.mapping {
			t.Errorf("location %d: got id=%d addr=%#x mapping=%d, want id=%d addr=%#x mapping=%d",
				i, l.ID, l.Address, l.Mapping.ID, want.id, want.addr, want.mapping.ID)
		}
		// The file-relative address must be preserved.
		if got, want := fileOffset(l), fileOffset(origLocations[i]); got != want {
			t.Errorf("location %d: file-relative address changed: got %#x, want %#x", l.ID, got, want)
		}
	}

	// Shared locations must still be shared, and lines/functions intact.
	if rp.Sample[0].Location[0] != rp.Sample[1].Location[0] {
		t.Errorf("location shared between samples 0 and 1 is no longer shared")
	}
	if rp.Sample[0].Location[0] != rp.Location[0] || rp.Sample[0].Location[1] != rp.Location[1] {
		t.Errorf("sample locations do not reference the profile location table")
	}
	l2 := rp.Location[1]
	if len(l2.Line) != 2 || l2.Line[0].Function.Name != "inlined1" || l2.Line[1].Function.Name != "inlined2" {
		t.Errorf("inlined frames of location 2 not preserved: %+v", l2.Line)
	}
	if l2.Line[0].Line != 20 || l2.Line[0].Column != 3 || l2.Line[1].Line != 30 {
		t.Errorf("line info of location 2 not preserved: %+v", l2.Line)
	}
	if l2.Line[0].Function != rp.Function[1] || l2.Line[1].Function != rp.Function[2] {
		t.Errorf("location 2 lines do not reference the profile function table")
	}

	if err := rp.CheckValid(); err != nil {
		t.Errorf("relocated profile is invalid: %v", err)
	}

	// The relocated profile must survive a write/parse roundtrip.
	var buf bytes.Buffer
	if err := rp.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	pp, err := Parse(&buf)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := pp.String(), rp.String(); got != want {
		t.Errorf("roundtripped profile mismatch, got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRelocateMappingsErrors(t *testing.T) {
	testCases := []struct {
		name string
		plan map[uint64][]MappingSegment
	}{
		{
			name: "unknown mapping",
			plan: map[uint64][]MappingSegment{
				42: {{Start: 0x1000, Limit: 0x2000, NewStart: 0x5000}},
			},
		},
		{
			name: "gap between segments",
			plan: map[uint64][]MappingSegment{
				1: {
					{Start: 0x1000, Limit: 0x1800, NewStart: 0x5000},
					{Start: 0x2000, Limit: 0x3000, NewStart: 0x6000},
				},
			},
		},
		{
			name: "overlapping segments",
			plan: map[uint64][]MappingSegment{
				1: {
					{Start: 0x1000, Limit: 0x2800, NewStart: 0x5000},
					{Start: 0x2000, Limit: 0x3000, NewStart: 0x7000},
				},
			},
		},
		{
			name: "mapping start not covered",
			plan: map[uint64][]MappingSegment{
				1: {{Start: 0x1800, Limit: 0x3000, NewStart: 0x5000}},
			},
		},
		{
			name: "mapping end not covered",
			plan: map[uint64][]MappingSegment{
				1: {{Start: 0x1000, Limit: 0x2000, NewStart: 0x5000}},
			},
		},
		{
			name: "empty segment",
			plan: map[uint64][]MappingSegment{
				1: {
					{Start: 0x1000, Limit: 0x2000, NewStart: 0x5000},
					{Start: 0x2000, Limit: 0x2000, NewStart: 0x6000},
					{Start: 0x2000, Limit: 0x3000, NewStart: 0x6000},
				},
			},
		},
		{
			name: "target overlaps untouched mapping",
			plan: map[uint64][]MappingSegment{
				1: {{Start: 0x1000, Limit: 0x3000, NewStart: 0x8800}},
			},
		},
		{
			name: "target segments overlap each other",
			plan: map[uint64][]MappingSegment{
				1: {
					{Start: 0x1000, Limit: 0x2000, NewStart: 0x5000},
					{Start: 0x2000, Limit: 0x3000, NewStart: 0x5800},
				},
			},
		},
		{
			name: "new start overflows",
			plan: map[uint64][]MappingSegment{
				1: {{Start: 0x1000, Limit: 0x3000, NewStart: math.MaxUint64 - 0x1000}},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			p := relocationTestProfile()
			orig := p.String()
			if _, err := p.RelocateMappings(tc.plan); err == nil {
				t.Errorf("RelocateMappings(%v) succeeded, want error", tc.plan)
			}
			if got := p.String(); got != orig {
				t.Errorf("original profile modified, got:\n%s\nwant:\n%s", got, orig)
			}
		})
	}
}

func TestRelocateMappingsOffsetOverflow(t *testing.T) {
	p := relocationTestProfile()
	p.Mapping[0].Offset = math.MaxUint64 - 0x10
	orig := p.String()
	plan := map[uint64][]MappingSegment{
		1: {
			{Start: 0x1000, Limit: 0x2000, NewStart: 0x5000},
			{Start: 0x2000, Limit: 0x3000, NewStart: 0x6000},
		},
	}
	if _, err := p.RelocateMappings(plan); err == nil {
		t.Errorf("RelocateMappings succeeded, want file offset overflow error")
	}
	if got := p.String(); got != orig {
		t.Errorf("original profile modified, got:\n%s\nwant:\n%s", got, orig)
	}
}

func TestRelocateMappingsLocationOutOfRange(t *testing.T) {
	p := relocationTestProfile()
	// Move a location of the relocated mapping outside of the mapping.
	p.Location[0].Address = 0x3500
	orig := p.String()
	plan := map[uint64][]MappingSegment{
		1: {{Start: 0x1000, Limit: 0x3000, NewStart: 0x5000}},
	}
	if _, err := p.RelocateMappings(plan); err == nil {
		t.Errorf("RelocateMappings succeeded, want location out of range error")
	}
	if got := p.String(); got != orig {
		t.Errorf("original profile modified, got:\n%s\nwant:\n%s", got, orig)
	}
}
