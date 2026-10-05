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
	"reflect"
	"testing"
)

const testBuildID = "deadbeef"

// newTestMapping returns a mapping with the given start, offset and
// (file-relative) limit, all sharing testBuildID.
func newTestMapping(id uint64, start, limit, offset uint64) *Mapping {
	return &Mapping{
		ID:      id,
		Start:   start,
		Limit:   limit,
		Offset:  offset,
		File:    "binary",
		BuildID: testBuildID,
	}
}

func testFunction(id uint64, name, file string, startLine int64) *Function {
	return &Function{
		ID:         id,
		Name:       name,
		SystemName: name,
		Filename:   file,
		StartLine:  startLine,
	}
}

// symbolizeChain builds an inline chain for a source location:
// leaf/inline frames first, outermost frame last, matching the order
// produced by the symbolizer.
func symbolizeChain(funcs ...*Function) []Line {
	lines := make([]Line, len(funcs))
	for i, f := range funcs {
		lines[i] = Line{Function: f, Line: 100 + int64(i), Column: int64(i + 1)}
	}
	return lines
}

// sourceProfileWith returns an already-symbolized profile whose
// locations map file-relative addresses (start=1000, offset=0) to the
// given chains.
func sourceProfileWith(chains map[uint64][]Line) *Profile {
	m := newTestMapping(1, 0x1000, 0x5000, 0)
	locs := make([]*Location, 0, len(chains))
	funcs := map[uint64]*Function{}
	for addr, lines := range chains {
		for _, ln := range lines {
			if ln.Function != nil {
				funcs[ln.Function.ID] = ln.Function
			}
		}
		locs = append(locs, &Location{
			ID:      addr,
			Mapping: m,
			Address: addr,
			Line:    lines,
		})
	}
	p := &Profile{
		SampleType: []*ValueType{{Type: "cpu", Unit: "cycles"}},
		Mapping:    []*Mapping{m},
		Location:   locs,
	}
	for _, f := range funcs {
		p.Function = append(p.Function, f)
	}
	return p
}

// targetLocation is a target location given by its file-relative
// address (relative to its mapping start) and its initial chain.
type targetLocation struct {
	id       uint64
	relative uint64
	line     []Line
}

// targetProfileWith returns an unsymbolized profile loaded at base
// with the given file-relative locations.
func targetProfileWith(base, limit, mappingOffset uint64, funcs []*Function, locs []targetLocation) *Profile {
	m := newTestMapping(1, base, base+limit, mappingOffset)
	p := &Profile{
		SampleType: []*ValueType{{Type: "cpu", Unit: "cycles"}},
		Mapping:    []*Mapping{m},
		Function:   funcs,
	}
	sampleLocs := make([]*Location, 0, len(locs))
	for _, tl := range locs {
		loc := &Location{
			ID:      tl.id,
			Mapping: m,
			Address: base + tl.relative,
			Line:    tl.line,
		}
		p.Location = append(p.Location, loc)
		sampleLocs = append(sampleLocs, loc)
	}
	p.Sample = []*Sample{{
		Location: sampleLocs,
		Value:    []int64{42},
		Label:    map[string][]string{"tag": {"v"}},
	}}
	return p
}

func TestCompleteSymbolsASLR(t *testing.T) {
	// The source is loaded at 0x1000 while the target is relocated by
	// ASLR to 0xa000; only BuildID + file-relative address should matter.
	inline := testFunction(1, "inline", "inline.cc", 10)
	leaf := testFunction(2, "leaf", "leaf.cc", 20)
	src := sourceProfileWith(map[uint64][]Line{
		0x1100: symbolizeChain(inline, leaf),
		0x1200: symbolizeChain(leaf),
	})

	target := targetProfileWith(0xa000, 0x4000, 0, nil, []targetLocation{
		{id: 1, relative: 0x100}, // empty chain
		{id: 2, relative: 0x200}, // empty chain
		{id: 3, relative: 0x300}, // no candidate
	})

	result, err := target.CompleteSymbols(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Completed) != 2 || len(result.Unmatched) != 1 || len(result.Conflicts) != 0 {
		t.Fatalf("got completed=%d unmatched=%d conflicts=%d, want 2/1/0",
			len(result.Completed), len(result.Unmatched), len(result.Conflicts))
	}
	if result.Unmatched[0].ID != 3 {
		t.Errorf("unmatched location id = %d, want 3", result.Unmatched[0].ID)
	}

	completed := result.Completed[0]
	if completed.ID != 1 {
		t.Fatalf("first completed id = %d, want 1", completed.ID)
	}
	if len(completed.Line) != 2 {
		t.Fatalf("inline chain length = %d, want 2", len(completed.Line))
	}
	if got := completed.Line[0].Function.Name; got != "inline" {
		t.Errorf("frame 0 function = %q, want inline", got)
	}
	if got := completed.Line[1].Function.Name; got != "leaf" {
		t.Errorf("frame 1 function = %q, want leaf", got)
	}
	if completed.Line[0].Line != 100 || completed.Line[1].Line != 101 {
		t.Errorf("line numbers not preserved in order: %+v", completed.Line)
	}
	if completed.Address != 0xa100 {
		t.Errorf("address changed: %#x, want %#x", completed.Address, uint64(0xa100))
	}

	// Addresses, values and labels must stay untouched.
	sample := target.Sample[0]
	if sample.Value[0] != 42 || sample.Label["tag"][0] != "v" {
		t.Errorf("sample values/labels changed: %+v", sample)
	}
	for _, loc := range target.Location {
		if loc.Mapping != target.Mapping[0] {
			t.Errorf("location mapping pointer changed")
		}
	}

	if err := target.CheckValid(); err != nil {
		t.Fatalf("completed profile is invalid: %v", err)
	}
}

func TestCompleteSymbolsTwoSourcesAgree(t *testing.T) {
	leaf := testFunction(1, "leaf", "leaf.cc", 20)
	chain := symbolizeChain(leaf)
	src1 := sourceProfileWith(map[uint64][]Line{0x1100: chain})
	src2 := sourceProfileWith(map[uint64][]Line{0x1100: chain})
	// Different function IDs in the two sources must not matter.
	src2.Location[0].Line[0].Function = testFunction(99, "leaf", "leaf.cc", 20)

	target := targetProfileWith(0x2000, 0x4000, 0, nil, []targetLocation{
		{id: 1, relative: 0x100},
	})
	result, err := target.CompleteSymbols(src1, src2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Completed) != 1 || len(result.Conflicts) != 0 || len(result.Unmatched) != 0 {
		t.Fatalf("got completed=%d conflicts=%d unmatched=%d, want 1/0/0",
			len(result.Completed), len(result.Conflicts), len(result.Unmatched))
	}
	if got := target.Location[0].Line[0].Function.Name; got != "leaf" {
		t.Errorf("function = %q, want leaf", got)
	}
}

func TestCompleteSymbolsTwoSourcesConflict(t *testing.T) {
	leaf1 := testFunction(1, "leaf", "leaf.cc", 20)
	leaf2 := testFunction(2, "other", "leaf.cc", 20)
	src1 := sourceProfileWith(map[uint64][]Line{0x1100: symbolizeChain(leaf1)})
	src2 := sourceProfileWith(map[uint64][]Line{0x1100: symbolizeChain(leaf2)})

	target := targetProfileWith(0x2000, 0x4000, 0, nil, []targetLocation{
		{id: 1, relative: 0x100},
	})
	before := target.Location[0]
	result, err := target.CompleteSymbols(src1, src2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(result.Conflicts))
	}
	c := result.Conflicts[0]
	if c.Location.ID != 1 || c.Field != FunctionName {
		t.Errorf("conflict = location %d field %v, want location 1 field %v",
			c.Location.ID, c.Field, FunctionName)
	}
	wantCandidates := []string{"leaf", "other"}
	if !reflect.DeepEqual(c.Candidates, wantCandidates) {
		t.Errorf("candidates = %v, want %v", c.Candidates, wantCandidates)
	}
	if before != target.Location[0] || len(target.Location[0].Line) != 0 {
		t.Errorf("conflicting location was modified")
	}
	if len(target.Function) != 0 {
		t.Errorf("functions were imported despite the conflict: %v", target.Function)
	}
}

func TestCompleteSymbolsExistingConflict(t *testing.T) {
	leaf := testFunction(1, "leaf", "leaf.cc", 20)
	src := sourceProfileWith(map[uint64][]Line{0x1100: symbolizeChain(leaf)})

	existingFn := testFunction(7, "stale", "", 0)
	target := targetProfileWith(0x2000, 0x4000, 0, []*Function{existingFn}, []targetLocation{
		{id: 1, relative: 0x100, line: []Line{{Function: existingFn, Line: 5}}},
	})
	result, err := target.CompleteSymbols(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(result.Conflicts))
	}
	c := result.Conflicts[0]
	if c.Field != FunctionName || c.Existing != "stale" {
		t.Errorf("conflict field=%v existing=%q, want function name/stale", c.Field, c.Existing)
	}
	if target.Location[0].Line[0].Function.Name != "stale" ||
		target.Location[0].Line[0].Function.Filename != "" {
		t.Errorf("conflicting location was partially modified")
	}
}

func TestCompleteSymbolsFilenameConflict(t *testing.T) {
	leafSrc := testFunction(1, "leaf", "new/leaf.cc", 20)
	src := sourceProfileWith(map[uint64][]Line{0x1100: symbolizeChain(leafSrc)})

	leafDst := testFunction(3, "leaf", "old/leaf.cc", 20)
	target := targetProfileWith(0x2100, 0x4000, 0, []*Function{leafDst}, []targetLocation{
		{id: 1, relative: 0x100, line: []Line{{Function: leafDst, Line: 100}}},
	})
	result, err := target.CompleteSymbols(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(result.Conflicts))
	}
	c := result.Conflicts[0]
	if c.Field != Filename || c.Frame != 0 {
		t.Errorf("conflict = %v frame %d, want file name/0", c.Field, c.Frame)
	}
	if leafDst.Filename != "old/leaf.cc" {
		t.Errorf("target filename changed to %q", leafDst.Filename)
	}
}

func TestCompleteSymbolsNoBuildID(t *testing.T) {
	leaf := testFunction(1, "leaf", "leaf.cc", 20)
	src := sourceProfileWith(map[uint64][]Line{0x1100: symbolizeChain(leaf)})

	target := targetProfileWith(0x2200, 0x4000, 0, nil, []targetLocation{
		{id: 1, relative: 0x100},
	})
	target.Mapping[0].BuildID = ""
	result, err := target.CompleteSymbols(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Unmatched) != 1 || len(result.Completed) != 0 {
		t.Fatalf("got unmatched=%d completed=%d, want 1/0",
			len(result.Unmatched), len(result.Completed))
	}
}

func TestCompleteSymbolsPreservesSecondaryFields(t *testing.T) {
	// The target already carries a system name and a column that differ
	// from (or are absent in) the source. These are not core fields for
	// contradiction, so they are preserved, while the missing file name
	// is still filled.
	srcFn := &Function{ID: 1, Name: "leaf", SystemName: "_Z3foov", Filename: "leaf.cc", StartLine: 20}
	src := sourceProfileWith(map[uint64][]Line{
		0x1100: {{Function: srcFn, Line: 100}},
	})

	dstFn := &Function{ID: 5, Name: "leaf", SystemName: "custom-sys", StartLine: 20}
	target := targetProfileWith(0x2300, 0x4000, 0, []*Function{dstFn}, []targetLocation{
		{id: 1, relative: 0x100, line: []Line{{Function: dstFn, Line: 100, Column: 7}}},
	})
	result, err := target.CompleteSymbols(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("got %d conflicts, want 0", len(result.Conflicts))
	}
	if len(result.Completed) != 1 {
		t.Fatalf("got %d completed, want 1 (filename filled)", len(result.Completed))
	}
	if dstFn.SystemName != "custom-sys" {
		t.Errorf("existing system name overwritten: %q", dstFn.SystemName)
	}
	if dstFn.Filename != "leaf.cc" {
		t.Errorf("missing file name not filled: %q", dstFn.Filename)
	}
	if target.Location[0].Line[0].Column != 7 {
		t.Errorf("existing column changed: %d", target.Location[0].Line[0].Column)
	}
}

func TestCompleteSymbolsMissingFilenameOnly(t *testing.T) {
	// Only the filename is missing; function name and line number must
	// stay untouched and just the filename gets filled.
	leaf := testFunction(1, "leaf", "dir/leaf.cc", 20)
	src := sourceProfileWith(map[uint64][]Line{0x1100: symbolizeChain(leaf)})

	existing := testFunction(3, "leaf", "", 0)
	target := targetProfileWith(0x2000, 0x4000, 0, []*Function{existing}, []targetLocation{
		{id: 1, relative: 0x100, line: []Line{{Function: existing, Line: 100}}},
	})
	result, err := target.CompleteSymbols(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Completed) != 1 || len(result.Conflicts) != 0 {
		t.Fatalf("got completed=%d conflicts=%d, want 1/0",
			len(result.Completed), len(result.Conflicts))
	}
	ln := target.Location[0].Line[0]
	if ln.Function != existing {
		t.Errorf("existing function was replaced instead of completed")
	}
	if existing.Filename != "dir/leaf.cc" || existing.StartLine != 20 {
		t.Errorf("function not filled: %+v", existing)
	}
	if ln.Line != 100 {
		t.Errorf("existing line number changed: %d", ln.Line)
	}
	if len(target.Function) != 1 {
		t.Errorf("duplicate function imported, got %d functions", len(target.Function))
	}
}

func TestCompleteSymbolsSharedFunction(t *testing.T) {
	// The same function appears at two distinct addresses, including via
	// an inline chain; it must be imported once and shared.
	leaf := testFunction(1, "leaf", "leaf.cc", 20)
	inner := testFunction(2, "inner", "inner.cc", 30)
	src := sourceProfileWith(map[uint64][]Line{
		0x1100: symbolizeChain(leaf),
		0x1200: symbolizeChain(inner, leaf),
	})

	target := targetProfileWith(0x3000, 0x4000, 0, nil, []targetLocation{
		{id: 1, relative: 0x100},
		{id: 2, relative: 0x200},
	})
	if _, err := target.CompleteSymbols(src); err != nil {
		t.Fatal(err)
	}
	l1 := target.Location[0].Line[0]
	l2 := target.Location[1].Line[1]
	if l1.Function != l2.Function {
		t.Errorf("identical functions were not reused: %p vs %p", l1.Function, l2.Function)
	}
	if len(target.Function) != 2 {
		t.Fatalf("got %d functions, want 2 (leaf and inner deduplicated)", len(target.Function))
	}
	for _, f := range target.Function {
		if f.ID == 0 {
			t.Errorf("imported function has reserved id 0")
		}
	}
}

func TestCompleteSymbolsMappingFlags(t *testing.T) {
	leaf := testFunction(1, "leaf", "leaf.cc", 20)
	src := sourceProfileWith(map[uint64][]Line{0x1100: symbolizeChain(leaf, leaf)})

	// The target mapping claims to be fully symbolized; only one of its
	// three locations can be completed, so partial coverage must not be
	// reported as complete.
	target := targetProfileWith(0x4000, 0x4000, 0, nil, []targetLocation{
		{id: 1, relative: 0x100},
		{id: 2, relative: 0x200},
		{id: 3, relative: 0x300},
	})
	target.Mapping[0].HasFunctions = true
	target.Mapping[0].HasFilenames = true
	target.Mapping[0].HasLineNumbers = true

	if _, err := target.CompleteSymbols(src); err != nil {
		t.Fatal(err)
	}
	m := target.Mapping[0]
	if m.HasFunctions || m.HasFilenames || m.HasLineNumbers {
		t.Errorf("mapping reported as fully covered despite unmatched locations")
	}
	if !m.HasInlineFrames {
		t.Errorf("HasInlineFrames = false, want true (completed chain has 2 frames)")
	}
}

func TestCompleteSymbolsMappingOffset(t *testing.T) {
	// Source mapping starts at 0x1000 with offset 0x10000; the target
	// mapping starts at 0x5000 with the same offset. File-relative
	// addresses must still match.
	leaf := testFunction(1, "leaf", "leaf.cc", 20)
	src := &Profile{
		SampleType: []*ValueType{{Type: "cpu", Unit: "cycles"}},
		Function:   []*Function{leaf},
	}
	srcM := newTestMapping(1, 0x1000, 0x9000, 0x10000)
	src.Mapping = []*Mapping{srcM}
	// Relative address 0x200 -> absolute 0x1200.
	src.Location = []*Location{{ID: 1, Mapping: srcM, Address: 0x1200, Line: symbolizeChain(leaf)}}

	targetM := newTestMapping(1, 0x5000, 0xd000, 0x10000)
	target := &Profile{
		SampleType: []*ValueType{{Type: "cpu", Unit: "cycles"}},
		Mapping:    []*Mapping{targetM},
		Location:   []*Location{{ID: 1, Mapping: targetM, Address: 0x5200}},
		Sample:     []*Sample{{Value: []int64{1}}},
	}
	target.Sample[0].Location = []*Location{target.Location[0]}

	result, err := target.CompleteSymbols(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Completed) != 1 {
		t.Fatalf("got %d completed, want 1", len(result.Completed))
	}
}

func TestCompleteSymbolsInlineChainLengthConflict(t *testing.T) {
	inline := testFunction(1, "inline", "inline.cc", 10)
	leaf := testFunction(2, "leaf", "leaf.cc", 20)
	src := sourceProfileWith(map[uint64][]Line{
		0x1100: symbolizeChain(inline, leaf),
	})

	existing := testFunction(3, "something", "x.cc", 0)
	target := targetProfileWith(0x6000, 0x4000, 0, []*Function{existing}, []targetLocation{
		{id: 1, relative: 0x100, line: []Line{{Function: existing, Line: 7}}},
	})
	result, err := target.CompleteSymbols(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(result.Conflicts))
	}
	c := result.Conflicts[0]
	if c.Field != InlineChainLength || c.Frame != -1 {
		t.Errorf("conflict = %v frame %d, want inline chain length/-1", c.Field, c.Frame)
	}
	if len(target.Location[0].Line) != 1 {
		t.Errorf("conflicting chain length was changed")
	}
}

func TestCompleteSymbolsSourceChainLengthDisagree(t *testing.T) {
	// The two sources offer chains of different lengths for the same
	// address while the target chain is empty: that is a conflict, not a
	// reason to pick either source by input order.
	inline := testFunction(1, "inline", "inline.cc", 10)
	leaf := testFunction(2, "leaf", "leaf.cc", 20)
	src1 := sourceProfileWith(map[uint64][]Line{
		0x1100: symbolizeChain(inline, leaf),
	})
	src2 := sourceProfileWith(map[uint64][]Line{
		0x1100: symbolizeChain(leaf),
	})

	target := targetProfileWith(0x9000, 0x4000, 0, nil, []targetLocation{
		{id: 1, relative: 0x100},
	})
	result, err := target.CompleteSymbols(src2, src1) // reversed order on purpose
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(result.Conflicts))
	}
	c := result.Conflicts[0]
	if c.Field != InlineChainLength {
		t.Errorf("field = %v, want inline chain length", c.Field)
	}
	if c.Existing != "" {
		t.Errorf("existing = %q, want empty for an empty target chain", c.Existing)
	}
	if !reflect.DeepEqual(c.Candidates, []string{"1", "2"}) {
		t.Errorf("candidates = %v, want [1 2]", c.Candidates)
	}
	if len(target.Location[0].Line) != 0 {
		t.Errorf("conflicting empty chain was modified")
	}
}

func TestCompleteSymbolsRoundTrip(t *testing.T) {
	inline := testFunction(1, "inline", "inline.cc", 10)
	leaf := testFunction(2, "leaf", "leaf.cc", 20)
	src := sourceProfileWith(map[uint64][]Line{
		0x1100: symbolizeChain(inline, leaf),
		0x1200: symbolizeChain(leaf),
	})
	target := targetProfileWith(0x7000, 0x4000, 0, nil, []targetLocation{
		{id: 1, relative: 0x100},
		{id: 2, relative: 0x200},
		{id: 3, relative: 0x900},
	})

	if _, err := target.CompleteSymbols(src); err != nil {
		t.Fatal(err)
	}
	if err := target.CheckValid(); err != nil {
		t.Fatalf("invalid profile after completion: %v", err)
	}

	var buf bytes.Buffer
	if err := target.Write(&buf); err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.CheckValid(); err != nil {
		t.Fatalf("parsed profile invalid: %v", err)
	}

	got := parsed.Location
	if len(got) != len(target.Location) {
		t.Fatalf("location count = %d, want %d", len(got), len(target.Location))
	}
	loc1 := got[0]
	if len(loc1.Line) != 2 {
		t.Fatalf("inline chain lost in round trip: got %d frames", len(loc1.Line))
	}
	if loc1.Line[0].Function.Name != "inline" || loc1.Line[1].Function.Name != "leaf" {
		t.Errorf("inline chain order/content changed: %+v", loc1.Line)
	}
	// The shared leaf function must remain a single function after the
	// serialization round trip.
	if loc1.Line[1].Function.ID != got[1].Line[0].Function.ID {
		t.Errorf("shared function not shared after round trip")
	}
	if parsed.Sample[0].Value[0] != 42 || parsed.Sample[0].Label["tag"][0] != "v" {
		t.Errorf("sample values/labels changed during round trip")
	}
}

func TestCompleteSymbolsDoesNotMutateSources(t *testing.T) {
	leaf := testFunction(1, "leaf", "leaf.cc", 20)
	src := sourceProfileWith(map[uint64][]Line{
		0x1100: symbolizeChain(leaf),
	})

	var before bytes.Buffer
	if err := src.WriteUncompressed(&before); err != nil {
		t.Fatal(err)
	}

	target := targetProfileWith(0x8000, 0x4000, 0, nil, []targetLocation{
		{id: 1, relative: 0x100},
	})
	if _, err := target.CompleteSymbols(src); err != nil {
		t.Fatal(err)
	}

	// Mutate the completed result; this must not reach the source.
	target.Location[0].Line[0].Function.Name = "mutated"
	target.Location[0].Line[0].Function.Filename = "mutated.cc"

	var after bytes.Buffer
	if err := src.WriteUncompressed(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Bytes(), after.Bytes()) {
		t.Errorf("source profile was modified by completion")
	}
	if src.Location[0].Line[0].Function.Name != "leaf" {
		t.Errorf("source function name changed to %q", src.Location[0].Line[0].Function.Name)
	}
}
