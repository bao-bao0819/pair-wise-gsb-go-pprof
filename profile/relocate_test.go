package profile

import (
	"bytes"
	"math"
	"reflect"
	"testing"
)

func TestRelocateMappingsSplitAndSwap(t *testing.T) {
	p := relocationTestProfile()
	var sourceBefore bytes.Buffer
	if err := p.WriteUncompressed(&sourceBefore); err != nil {
		t.Fatalf("WriteUncompressed returned error: %v", err)
	}

	relocations := []MappingRelocation{
		{MappingID: 1, Start: 0x1000, Limit: 0x2000, NewStart: 0x8000},
		{MappingID: 1, Start: 0x2000, Limit: 0x3000, NewStart: 0x6000},
	}
	got, err := p.RelocateMappings(relocations)
	if err != nil {
		t.Fatalf("RelocateMappings returned error: %v", err)
	}
	var sourceAfter bytes.Buffer
	if err := p.WriteUncompressed(&sourceAfter); err != nil {
		t.Fatalf("WriteUncompressed returned error after relocation: %v", err)
	}
	if !bytes.Equal(sourceBefore.Bytes(), sourceAfter.Bytes()) {
		t.Fatal("successful relocation modified the source profile")
	}

	wantMappings := []struct {
		id              uint64
		start, limit    uint64
		offset          uint64
		file, buildID   string
		hasFunctions    bool
		hasFilenames    bool
		hasLineNumbers  bool
		hasInlineFrames bool
	}{
		{1, 0x8000, 0x9000, 0x100, "binary", "build-id", true, true, true, true},
		{3, 0x6000, 0x7000, 0x1100, "binary", "build-id", true, true, true, true},
		{2, 0x9000, 0xa000, 0x500, "other", "other-build", false, false, false, false},
	}
	if len(got.Mapping) != len(wantMappings) {
		t.Fatalf("got %d mappings, want %d", len(got.Mapping), len(wantMappings))
	}
	for i, want := range wantMappings {
		m := got.Mapping[i]
		if m.ID != want.id || m.Start != want.start || m.Limit != want.limit || m.Offset != want.offset ||
			m.File != want.file || m.BuildID != want.buildID ||
			m.HasFunctions != want.hasFunctions || m.HasFilenames != want.hasFilenames ||
			m.HasLineNumbers != want.hasLineNumbers || m.HasInlineFrames != want.hasInlineFrames {
			t.Errorf("mapping %d = %+v, want %+v", i, m, want)
		}
	}

	wantLocations := map[uint64]struct {
		address       uint64
		mappingStart  uint64
		mappingOffset uint64
		fileRelative  uint64
		functionIDs   []uint64
		lines         []int64
	}{
		10: {0x8000, 0x8000, 0x100, 0x100, []uint64{1, 2}, []int64{11, 12}},
		11: {0x6000, 0x6000, 0x1100, 0x1100, []uint64{2, 3}, []int64{21, 22}},
		12: {0x6fff, 0x6000, 0x1100, 0x20ff, nil, nil},
		13: {0x9100, 0x9000, 0x500, 0x600, []uint64{3}, []int64{31}},
	}
	locations := make(map[uint64]*Location, len(got.Location))
	for _, location := range got.Location {
		locations[location.ID] = location
		want, ok := wantLocations[location.ID]
		if !ok {
			t.Fatalf("unexpected location ID %d", location.ID)
		}
		if location.Address != want.address {
			t.Errorf("location %d address = %#x, want %#x", location.ID, location.Address, want.address)
		}
		if location.Mapping == nil {
			t.Fatalf("location %d has nil mapping", location.ID)
		}
		if location.Mapping.Start != want.mappingStart || location.Mapping.Offset != want.mappingOffset {
			t.Errorf("location %d mapping = [%#x,%#x) offset %#x, want start %#x offset %#x",
				location.ID, location.Mapping.Start, location.Mapping.Limit, location.Mapping.Offset,
				want.mappingStart, want.mappingOffset)
		}
		if gotRelative := location.Address - location.Mapping.Start + location.Mapping.Offset; gotRelative != want.fileRelative {
			t.Errorf("location %d file-relative address = %#x, want %#x", location.ID, gotRelative, want.fileRelative)
		}
		if len(location.Line) != len(want.functionIDs) {
			t.Fatalf("location %d has %d lines, want %d", location.ID, len(location.Line), len(want.functionIDs))
		}
		for i, functionID := range want.functionIDs {
			if location.Line[i].Function == nil || location.Line[i].Function.ID != functionID {
				t.Errorf("location %d line %d function = %v, want ID %d", location.ID, i, location.Line[i].Function, functionID)
			}
			if location.Line[i].Line != want.lines[i] {
				t.Errorf("location %d line %d = %d, want %d", location.ID, i, location.Line[i].Line, want.lines[i])
			}
		}
	}

	if got.Sample[0].Location[1] != got.Sample[1].Location[0] {
		t.Fatal("shared location was not preserved")
	}
	if got.Sample[0].Location[1] != locations[11] {
		t.Fatal("sample does not reference the relocated boundary location")
	}
	if got.Sample[0].Location[1].Address != 0x6000 || got.Sample[0].Location[1].Mapping.Start != 0x6000 {
		t.Fatal("boundary address did not enter the second relocated segment")
	}
	if err := got.CheckValid(); err != nil {
		t.Fatalf("relocated profile is invalid: %v", err)
	}

	var buf bytes.Buffer
	if err := got.Write(&buf); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	parsed, err := Parse(&buf)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if len(parsed.Mapping) != len(got.Mapping) || len(parsed.Location) != len(got.Location) || len(parsed.Sample) != len(got.Sample) {
		t.Fatal("round trip changed profile table sizes")
	}
	parsedMappings := make(map[uint64]*Mapping, len(parsed.Mapping))
	for _, mapping := range parsed.Mapping {
		parsedMappings[mapping.ID] = mapping
		want := got.Mapping[0]
		for _, candidate := range got.Mapping {
			if candidate.ID == mapping.ID {
				want = candidate
			}
		}
		if mapping.Start != want.Start || mapping.Limit != want.Limit || mapping.Offset != want.Offset ||
			mapping.File != want.File || mapping.BuildID != want.BuildID ||
			mapping.HasFunctions != want.HasFunctions || mapping.HasFilenames != want.HasFilenames ||
			mapping.HasLineNumbers != want.HasLineNumbers || mapping.HasInlineFrames != want.HasInlineFrames {
			t.Fatalf("round trip changed mapping %d", mapping.ID)
		}
	}
	parsedByID := make(map[uint64]*Location, len(parsed.Location))
	for _, location := range parsed.Location {
		parsedByID[location.ID] = location
	}
	for sampleIndex, sample := range parsed.Sample {
		for locationIndex, location := range sample.Location {
			want := got.Sample[sampleIndex].Location[locationIndex]
			if location != parsedByID[want.ID] || location.Address != want.Address ||
				location.Mapping != parsedMappings[want.Mapping.ID] ||
				location.Mapping.Start != want.Mapping.Start || location.Mapping.Limit != want.Mapping.Limit ||
				location.Mapping.Offset != want.Mapping.Offset {
				t.Fatalf("round trip changed sample %d location %d", sampleIndex, locationIndex)
			}
			if len(location.Line) != len(want.Line) {
				t.Fatalf("round trip changed sample %d location %d line count", sampleIndex, locationIndex)
			}
			for lineIndex, line := range location.Line {
				wantLine := want.Line[lineIndex]
				if line.Function.ID != wantLine.Function.ID || line.Line != wantLine.Line {
					t.Fatalf("round trip changed sample %d location %d line %d", sampleIndex, locationIndex, lineIndex)
				}
			}
		}
	}
	if err := parsed.CheckValid(); err != nil {
		t.Fatalf("parsed profile is invalid: %v", err)
	}
}

func TestRelocateMappingsRejectsInvalidInputWithoutModification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		adjust      func(*Profile)
		relocations []MappingRelocation
	}{
		{
			name: "unknown mapping",
			relocations: []MappingRelocation{
				{MappingID: 99, Start: 0x1000, Limit: 0x2000, NewStart: 0x6000},
			},
		},
		{
			name: "overlapping source intervals",
			relocations: []MappingRelocation{
				{MappingID: 1, Start: 0x1000, Limit: 0x2100, NewStart: 0x8000},
				{MappingID: 1, Start: 0x2000, Limit: 0x3000, NewStart: 0x6000},
			},
		},
		{
			name: "source gap",
			relocations: []MappingRelocation{
				{MappingID: 1, Start: 0x1000, Limit: 0x1fff, NewStart: 0x8000},
				{MappingID: 1, Start: 0x2001, Limit: 0x3000, NewStart: 0x6000},
			},
		},
		{
			name: "location outside mapping",
			adjust: func(p *Profile) {
				p.Location[0].Address = 0x3000
			},
			relocations: []MappingRelocation{
				{MappingID: 1, Start: 0x1000, Limit: 0x2000, NewStart: 0x8000},
				{MappingID: 1, Start: 0x2000, Limit: 0x3000, NewStart: 0x6000},
			},
		},
		{
			name: "destination overlap",
			relocations: []MappingRelocation{
				{MappingID: 1, Start: 0x1000, Limit: 0x2000, NewStart: 0x9800},
				{MappingID: 1, Start: 0x2000, Limit: 0x3000, NewStart: 0x6000},
			},
		},
		{
			name: "destination endpoint overflow",
			adjust: func(p *Profile) {
				p.Mapping[0].Limit = 0x1001
				for _, location := range p.Location {
					if location.Mapping == p.Mapping[0] && location.Address > 0x1000 {
						location.Address = 0x1000
					}
				}
			},
			relocations: []MappingRelocation{
				{MappingID: 1, Start: 0x1000, Limit: 0x1001, NewStart: math.MaxUint64},
			},
		},
		{
			name: "file offset overflow",
			adjust: func(p *Profile) {
				p.Mapping[0].Offset = math.MaxUint64
			},
			relocations: []MappingRelocation{
				{MappingID: 1, Start: 0x1000, Limit: 0x2000, NewStart: 0x8000},
				{MappingID: 1, Start: 0x2000, Limit: 0x3000, NewStart: 0x6000},
			},
		},
		{
			name: "segment file-offset endpoint overflow",
			adjust: func(p *Profile) {
				p.Mapping[0].Offset = math.MaxUint64 - 0x1fff
			},
			relocations: []MappingRelocation{
				{MappingID: 1, Start: 0x1000, Limit: 0x2000, NewStart: 0x8000},
				{MappingID: 1, Start: 0x2000, Limit: 0x3000, NewStart: 0x6000},
			},
		},
		{
			name: "location file-relative address overflow",
			adjust: func(p *Profile) {
				p.Mapping[0].Offset = math.MaxUint64
				for _, location := range p.Location {
					if location.Mapping == p.Mapping[0] {
						location.Address = 0x1000
					}
				}
				p.Location[1].Address = 0x1001
			},
			relocations: []MappingRelocation{
				{MappingID: 1, Start: 0x1000, Limit: 0x3000, NewStart: 0x40000},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := relocationTestProfile()
			want := relocationTestProfile()
			if tc.adjust != nil {
				tc.adjust(p)
				tc.adjust(want)
			}

			if _, err := p.RelocateMappings(tc.relocations); err == nil {
				t.Fatal("RelocateMappings succeeded, want error")
			}
			if !reflect.DeepEqual(p, want) {
				t.Fatal("failed relocation modified the profile")
			}
		})
	}
}

func relocationTestProfile() *Profile {
	mapping := &Mapping{
		ID:              1,
		Start:           0x1000,
		Limit:           0x3000,
		Offset:          0x100,
		File:            "binary",
		BuildID:         "build-id",
		HasFunctions:    true,
		HasFilenames:    true,
		HasLineNumbers:  true,
		HasInlineFrames: true,
	}
	otherMapping := &Mapping{
		ID:      2,
		Start:   0x9000,
		Limit:   0xa000,
		Offset:  0x500,
		File:    "other",
		BuildID: "other-build",
	}
	functions := []*Function{
		{ID: 1, Name: "leaf", SystemName: "leaf", Filename: "leaf.go"},
		{ID: 2, Name: "inline", SystemName: "inline", Filename: "inline.go"},
		{ID: 3, Name: "root", SystemName: "root", Filename: "root.go"},
	}
	locations := []*Location{
		{
			ID:      10,
			Mapping: mapping,
			Address: 0x1000,
			Line: []Line{
				{Function: functions[0], Line: 11},
				{Function: functions[1], Line: 12},
			},
		},
		{
			ID:      11,
			Mapping: mapping,
			Address: 0x2000,
			Line: []Line{
				{Function: functions[1], Line: 21},
				{Function: functions[2], Line: 22},
			},
		},
		{
			ID:      12,
			Mapping: mapping,
			Address: 0x2fff,
		},
		{
			ID:      13,
			Mapping: otherMapping,
			Address: 0x9100,
			Line: []Line{
				{Function: functions[2], Line: 31},
			},
		},
	}

	return &Profile{
		SampleType: []*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample: []*Sample{
			{Location: []*Location{locations[0], locations[1]}, Value: []int64{10}},
			{Location: []*Location{locations[1], locations[2], locations[3]}, Value: []int64{20}},
		},
		Mapping:  []*Mapping{mapping, otherMapping},
		Location: locations,
		Function: functions,
	}
}
