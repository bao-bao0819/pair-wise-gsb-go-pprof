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
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/google/pprof/internal/proftest"
)

const (
	buildIDMain = "deadbeef"
	buildIDLib  = "cafef00d"
)

func testMapping(id uint64, start, limit, offset uint64, buildID string) *Mapping {
	return &Mapping{
		ID:      id,
		Start:   start,
		Limit:   limit,
		Offset:  offset,
		File:    "binary",
		BuildID: buildID,
	}
}

func testFunc(id uint64, name, file string, startLine int64) *Function {
	return &Function{
		ID:         id,
		Name:       name,
		SystemName: name,
		Filename:   file,
		StartLine:  startLine,
	}
}

func testLine(f *Function, line int64) Line {
	return Line{Function: f, Line: line}
}

func locIDs(locs []*Location) []uint64 {
	ids := make([]uint64, len(locs))
	for i, l := range locs {
		ids[i] = l.ID
	}
	return ids
}

// completionFixture builds a target profile and two independently
// symbolized source profiles. All three mappings share buildIDMain but
// are loaded at unrelated bases, so matching is possible only by build
// ID plus file-relative address. File-relative addresses used below
// are 0x100..0x600 plus 0xf00.
//
// Target base: 0x5000. Source A base: 0x10000. Source B base:
// 0x800000 (offset 0x1000, exercising offset normalization).
func completionFixture() (target, srcA, srcB *Profile) {
	tm := testMapping(1, 0x5000, 0x9000, 0x0, buildIDMain)
	noIDMap := &Mapping{ID: 2, Start: 0xa000, Limit: 0xb000, File: "anon"}
	tf1 := testFunc(1, "stale_name", "target.c", 10)
	tf1.SystemName = "target_fun" // only Name disagrees with the sources
	tf2 := testFunc(2, "partial", "", 30)
	tf3 := testFunc(3, "existing", "existing.c", 40)

	target = &Profile{
		SampleType: []*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Function:   []*Function{tf1, tf2, tf3},
		Mapping:    []*Mapping{tm, noIDMap},
		Location: []*Location{
			{ID: 1, Address: 0x5100, Mapping: tm}, // rel 0x100: both sources agree, 2 frames
			{
				ID:      2,
				Address: 0x5200,
				Mapping: tm,
				Line:    []Line{testLine(tf1, 20)},
			}, // rel 0x200: existing name contradicts sources
			{ID: 3, Address: 0x5f00, Mapping: tm}, // rel 0xf00: unmatched
			{
				ID:      4,
				Address: 0x5300,
				Mapping: tm,
				Line:    []Line{testLine(tf2, 0)},
			}, // rel 0x300: only filename and line missing
			{
				ID:      5,
				Address: 0x5400,
				Mapping: tm,
				Line:    []Line{testLine(tf3, 41)},
			}, // rel 0x400: target 1 frame, sources 2 frames
			{ID: 6, Address: 0x5500, Mapping: tm},      // rel 0x500: shared function
			{ID: 7, Address: 0x5600, Mapping: tm},      // rel 0x600: sources unsymbolized
			{ID: 8, Address: 0xa100, Mapping: noIDMap}, // mapping has no build ID
		},
		Sample: []*Sample{{
			Value:    []int64{123},
			Label:    map[string][]string{"trace": {"abc"}},
			NumLabel: map[string][]int64{"pid": {7}},
			NumUnit:  map[string][]string{"pid": {"id"}},
		}},
	}
	target.Sample[0].Location = []*Location{target.Location[0]}

	// Source A: base 0x10000, offset 0.
	mA := testMapping(1, 0x10000, 0x20000, 0x0, buildIDMain)
	srcA = &Profile{
		SampleType: []*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Mapping:    []*Mapping{mA},
		Function: []*Function{
			testFunc(100, "leaf_fun", "leaf.c", 1),
			testFunc(101, "inline_fun", "inline.c", 5),
			testFunc(102, "target_fun", "target.c", 10),
			testFunc(103, "partial", "partial.c", 30),
			testFunc(104, "other", "other.c", 60),
			testFunc(105, "shared_fun", "shared.c", 90),
		},
		Location: []*Location{
			{
				ID:      10,
				Address: 0x10100, // rel 0x100
				Mapping: mA,
				Line: []Line{
					testLine(testFunc(101, "inline_fun", "inline.c", 5), 6),
					testLine(testFunc(100, "leaf_fun", "leaf.c", 1), 2),
				},
			},
			{
				ID:      11,
				Address: 0x10200, // rel 0x200
				Mapping: mA,
				Line:    []Line{testLine(testFunc(102, "target_fun", "target.c", 10), 20)},
			},
			{
				ID:      12,
				Address: 0x10300, // rel 0x300
				Mapping: mA,
				Line:    []Line{testLine(testFunc(103, "partial", "partial.c", 30), 35)},
			},
			{
				ID:      13,
				Address: 0x10400, // rel 0x400
				Mapping: mA,
				Line: []Line{
					testLine(testFunc(104, "other", "other.c", 60), 61),
					testLine(testFunc(104, "other", "other.c", 60), 62),
				},
			},
			{
				ID:      14,
				Address: 0x10500, // rel 0x500
				Mapping: mA,
				Line:    []Line{testLine(testFunc(105, "shared_fun", "shared.c", 90), 100)},
			},
			{ID: 15, Address: 0x10600, Mapping: mA}, // rel 0x600: unsymbolized
		},
	}

	// Source B: base 0x800000 with offset 0x1000. Its file-relative
	// addresses are address - start + offset, identical to source A and
	// the target despite the very different load base.
	mB := testMapping(7, 0x800000, 0x900000, 0x1000, buildIDMain)
	srcB = &Profile{
		SampleType: []*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Mapping:    []*Mapping{mB},
		Function: []*Function{
			testFunc(200, "leaf_fun", "leaf.c", 1),
			testFunc(201, "inline_fun", "inline.c", 5),
			testFunc(202, "target_fun", "target.c", 10),
			testFunc(203, "partial", "partial.c", 30),
			testFunc(204, "other", "other.c", 60),
			testFunc(205, "shared_fun", "shared.c", 90),
			testFunc(206, "shared_fun", "shared.c", 90), // distinct object, same contents
		},
		Location: []*Location{
			{
				ID:      70,
				Address: 0x7ff100, // rel 0x100
				Mapping: mB,
				Line: []Line{
					testLine(testFunc(201, "inline_fun", "inline.c", 5), 6),
					testLine(testFunc(200, "leaf_fun", "leaf.c", 1), 2),
				},
			},
			{
				ID:      71,
				Address: 0x7ff200, // rel 0x200
				Mapping: mB,
				Line:    []Line{testLine(testFunc(202, "target_fun", "target.c", 10), 20)},
			},
			{
				ID:      72,
				Address: 0x7ff300, // rel 0x300
				Mapping: mB,
				Line:    []Line{testLine(testFunc(203, "partial", "partial.c", 30), 35)},
			},
			{
				ID:      73,
				Address: 0x7ff400, // rel 0x400
				Mapping: mB,
				Line: []Line{
					testLine(testFunc(204, "other", "other.c", 60), 61),
					testLine(testFunc(204, "other", "other.c", 60), 62),
				},
			},
			{
				ID:      74,
				Address: 0x7ff500, // rel 0x500
				Mapping: mB,
				Line:    []Line{testLine(testFunc(206, "shared_fun", "shared.c", 90), 100)},
			},
			{ID: 75, Address: 0x7ff600, Mapping: mB}, // rel 0x600: unsymbolized
		},
	}

	return target, srcA, srcB
}

func TestCompleteSymbols(t *testing.T) {
	target, srcA, srcB := completionFixture()
	targetSnap := proftest.EncodeJSON(target)
	srcASnap := proftest.EncodeJSON(srcA)
	srcBSnap := proftest.EncodeJSON(srcB)

	res, err := CompleteSymbols(target, srcA, srcB)
	if err != nil {
		t.Fatalf("CompleteSymbols: %v", err)
	}
	if err := res.Profile.CheckValid(); err != nil {
		t.Fatalf("completed profile is invalid: %v", err)
	}

	if got, want := locIDs(res.Completed), []uint64{1, 4, 6}; !reflect.DeepEqual(got, want) {
		t.Errorf("completed locations = %v, want %v", got, want)
	}
	if got, want := locIDs(res.Unmatched), []uint64{3, 7, 8}; !reflect.DeepEqual(got, want) {
		t.Errorf("unmatched locations = %v, want %v", got, want)
	}

	if len(res.Conflicts) != 2 {
		t.Fatalf("got %d conflicts, want 2: %+v", len(res.Conflicts), res.Conflicts)
	}
	byLoc := map[uint64]SymbolConflict{}
	for _, c := range res.Conflicts {
		byLoc[c.Location.ID] = c
	}
	c2 := byLoc[2]
	if c2.Field != FunctionName || c2.Existing != "stale_name" || c2.Frame != 0 {
		t.Errorf("location 2 conflict = %+v, want function name conflict on stale_name", c2)
	}
	if !reflect.DeepEqual(c2.Candidates, []string{"stale_name", "target_fun"}) {
		t.Errorf("location 2 candidates = %v", c2.Candidates)
	}
	c5 := byLoc[5]
	if c5.Field != InlineChainStructure || c5.Existing != "1" ||
		!reflect.DeepEqual(c5.Candidates, []string{"2"}) {
		t.Errorf("location 5 conflict = %+v, want inline chain structure conflict", c5)
	}

	p := res.Profile

	// Location 1: full inline chain copied in order, address unchanged.
	l1 := p.Location[0]
	if l1.Address != 0x5100 {
		t.Errorf("location 1 address changed to %#x", l1.Address)
	}
	if len(l1.Line) != 2 {
		t.Fatalf("location 1 has %d frames, want 2", len(l1.Line))
	}
	if got := l1.Line[0].Function.Name; got != "inline_fun" {
		t.Errorf("frame 0 = %q, want inline_fun (chain order preserved)", got)
	}
	if got := l1.Line[1].Function.Name; got != "leaf_fun" {
		t.Errorf("frame 1 = %q, want leaf_fun", got)
	}
	if l1.Line[0].Line != 6 || l1.Line[1].Line != 2 {
		t.Errorf("line numbers not copied: %+v", l1.Line)
	}

	// The conflicting location 2 is left byte-for-byte as it was.
	l2 := p.Location[1]
	if len(l2.Line) != 1 || l2.Line[0].Function.Name != "stale_name" || l2.Line[0].Line != 20 {
		t.Errorf("conflicting location 2 was modified: %+v", l2.Line)
	}

	// Location 4: existing fields kept, missing filename and line added.
	l4 := p.Location[3]
	f4 := l4.Line[0].Function
	if f4.Name != "partial" || f4.StartLine != 30 {
		t.Errorf("location 4 existing function fields changed: %+v", f4)
	}
	if f4.Filename != "partial.c" {
		t.Errorf("location 4 filename = %q, want partial.c", f4.Filename)
	}
	if l4.Line[0].Line != 35 {
		t.Errorf("location 4 line = %d, want 35", l4.Line[0].Line)
	}

	// Imported functions never alias a source object.
	l6 := p.Location[5]
	shared := l6.Line[0].Function
	if shared.Name != "shared_fun" || shared.Filename != "shared.c" || l6.Line[0].Line != 100 {
		t.Errorf("location 6 not completed as expected: %+v", l6.Line)
	}
	for _, src := range []*Profile{srcA, srcB} {
		for _, f := range src.Function {
			if f == shared {
				t.Errorf("imported function aliases a source function")
			}
		}
	}

	// Identical function contents are interned to a single table entry,
	// and every reference resolves to that same object.
	countByKey := map[functionKey]int{}
	for _, f := range p.Function {
		countByKey[f.key()]++
	}
	for k, n := range countByKey {
		if n != 1 {
			t.Errorf("function %+v appears %d times in the table", k, n)
		}
	}
	leafKey := l1.Line[1].Function.key()
	for _, f := range p.Function {
		if f.key() == leafKey && f != l1.Line[1].Function {
			t.Errorf("leaf_fun not interned to one object")
		}
	}

	// Sample value and labels survive; the sample references the
	// completed location 1.
	s := p.Sample[0]
	if !reflect.DeepEqual(s.Value, []int64{123}) {
		t.Errorf("sample value changed: %v", s.Value)
	}
	if !reflect.DeepEqual(s.Label, map[string][]string{"trace": {"abc"}}) {
		t.Errorf("sample label changed: %v", s.Label)
	}
	if !reflect.DeepEqual(s.NumLabel, map[string][]int64{"pid": {7}}) {
		t.Errorf("numeric label changed: %v", s.NumLabel)
	}
	if s.Location[0] != l1 {
		t.Errorf("sample no longer references completed location 1")
	}

	// Mapping flags reflect real coverage: only part of the main
	// mapping got data, so the coverage flags stay false while inline
	// presence is true thanks to location 1.
	main := p.Mapping[0]
	if main.HasFunctions || main.HasFilenames || main.HasLineNumbers {
		t.Errorf("partially covered mapping marked as fully covered: %+v", main)
	}
	if !main.HasInlineFrames {
		t.Errorf("HasInlineFrames should be set (location 1 has 2 frames)")
	}

	if !bytes.Equal(targetSnap, proftest.EncodeJSON(target)) {
		t.Errorf("target profile was modified")
	}
	if !bytes.Equal(srcASnap, proftest.EncodeJSON(srcA)) {
		t.Errorf("source A was modified")
	}
	if !bytes.Equal(srcBSnap, proftest.EncodeJSON(srcB)) {
		t.Errorf("source B was modified")
	}
}

// TestCompleteSymbolsOrderIndependent verifies that swapping the
// source order does not change the result or the conflicts.
func TestCompleteSymbolsOrderIndependent(t *testing.T) {
	t1, a1, b1 := completionFixture()
	t2, a2, b2 := completionFixture()
	r1, err := CompleteSymbols(t1, a1, b1)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := CompleteSymbols(t2, b2, a2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(proftest.EncodeJSON(r1.Profile), proftest.EncodeJSON(r2.Profile)) {
		t.Errorf("completed profiles differ with swapped source order")
	}
	if !reflect.DeepEqual(conflictKeys(r1), conflictKeys(r2)) {
		t.Errorf("conflicts differ with swapped source order:\n%v\n%v",
			conflictKeys(r1), conflictKeys(r2))
	}
}

func conflictKeys(r *SymbolCompletion) []string {
	var keys []string
	for _, c := range r.Conflicts {
		cands := append([]string(nil), c.Candidates...)
		sort.Strings(cands)
		keys = append(keys, fmt.Sprintf("%d/%s/%s/%v",
			c.Location.ID, c.Field, c.Existing, cands))
	}
	sort.Strings(keys)
	return keys
}

// TestCompleteSymbolsRoundtripAndIsolation serializes the result,
// re-parses it (Parse runs CheckValid), and checks equality. It then
// mutates the result and ensures neither input profile changes.
func TestCompleteSymbolsRoundtripAndIsolation(t *testing.T) {
	target, srcA, srcB := completionFixture()
	res, err := CompleteSymbols(target, srcA, srcB)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := res.Profile.Write(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	reparsed, err := Parse(&buf)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := reparsed.CheckValid(); err != nil {
		t.Fatalf("reparsed profile invalid: %v", err)
	}
	if !bytes.Equal(proftest.EncodeJSON(res.Profile), proftest.EncodeJSON(reparsed)) {
		t.Errorf("roundtrip changed the profile")
	}

	targetSnap := proftest.EncodeJSON(target)
	srcASnap := proftest.EncodeJSON(srcA)
	srcBSnap := proftest.EncodeJSON(srcB)

	for _, l := range res.Profile.Location {
		l.Address = 0
		for i := range l.Line {
			l.Line[i].Function.Name = "MUTATED"
			l.Line[i].Function.Filename = "MUTATED.c"
			l.Line[i].Line = 0
		}
	}
	for _, s := range res.Profile.Sample {
		s.Value[0] = -1
		s.Label = map[string][]string{"x": {"y"}}
	}

	if !bytes.Equal(targetSnap, proftest.EncodeJSON(target)) {
		t.Errorf("mutating the result modified the target profile")
	}
	if !bytes.Equal(srcASnap, proftest.EncodeJSON(srcA)) {
		t.Errorf("mutating the result modified source A")
	}
	if !bytes.Equal(srcBSnap, proftest.EncodeJSON(srcB)) {
		t.Errorf("mutating the result modified source B")
	}
}

// TestCompleteSymbolsSourceConflict checks that two sources offering
// different candidates conflict in either input order and leave the
// target untouched.
func TestCompleteSymbolsSourceConflict(t *testing.T) {
	// Target mapping at 0x1000; relative address of the lone location is
	// 0x500.
	tm := testMapping(1, 0x1000, 0x9000, 0, buildIDMain)
	target := &Profile{
		SampleType: []*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Mapping:    []*Mapping{tm},
		Location:   []*Location{{ID: 1, Address: 0x1500, Mapping: tm}},
	}

	// Source A at 0x10000 with independent objects.
	ma := testMapping(1, 0x10000, 0x20000, 0, buildIDMain)
	srcA := &Profile{
		SampleType: []*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Mapping:    []*Mapping{ma},
		Function:   []*Function{testFunc(1, "fun_a", "a.c", 1)},
		Location: []*Location{{
			ID:      1,
			Address: 0x10500, // rel 0x500
			Mapping: ma,
			Line:    []Line{testLine(testFunc(1, "fun_a", "a.c", 1), 11)},
		}},
	}
	// Source B at yet another base with a non-zero offset.
	mb := testMapping(2, 0x30000, 0x40000, 0x1000, buildIDMain)
	srcB := &Profile{
		SampleType: []*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Mapping:    []*Mapping{mb},
		Function:   []*Function{testFunc(1, "fun_b", "b.c", 1)},
		Location: []*Location{{
			ID:      2,
			Address: 0x30500 - 0x1000, // address - start + offset == 0x500
			Mapping: mb,
			Line:    []Line{testLine(testFunc(1, "fun_b", "b.c", 1), 22)},
		}},
	}

	check := func(t *testing.T, sources ...*Profile) {
		t.Helper()
		tgt := target.Copy()
		res, err := CompleteSymbols(tgt, sources...)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Completed) != 0 {
			t.Errorf("location filled despite source conflict: %+v", res.Completed)
		}
		if len(res.Conflicts) == 0 {
			t.Fatalf("expected conflicts, got none")
		}
		fields := map[SymbolField]bool{}
		for _, c := range res.Conflicts {
			fields[c.Field] = true
		}
		for _, f := range []SymbolField{FunctionName, FunctionFilename, LineNumber} {
			if !fields[f] {
				t.Errorf("missing %s conflict; got %+v", f, res.Conflicts)
			}
		}
		if len(res.Profile.Location[0].Line) != 0 {
			t.Errorf("conflicting target was modified")
		}
	}

	t.Run("a-b", func(t *testing.T) { check(t, srcA, srcB) })
	t.Run("b-a", func(t *testing.T) { check(t, srcB, srcA) })
}

// TestCompleteSymbolsFieldByField checks that each symbol field is
// filled independently and that an existing empty filename is picked up
// even when other fields are already present.
func TestCompleteSymbolsFieldByField(t *testing.T) {
	m := testMapping(1, 0x1000, 0x9000, 0, buildIDMain)
	ms := testMapping(1, 0x4000, 0x9000, 0, buildIDMain)

	// Target has name and system name but no filename, and no line.
	tf := &Function{ID: 1, Name: "fn", SystemName: "fn.sys", Filename: ""}
	target := &Profile{
		SampleType: []*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Function:   []*Function{tf},
		Mapping:    []*Mapping{m},
		Location: []*Location{{
			ID: 1, Address: 0x1500, Mapping: m,
			Line: []Line{{Function: tf}},
		}},
	}
	sf := &Function{ID: 9, Name: "fn", SystemName: "fn.sys", Filename: "dir/fn.go", StartLine: 3}
	source := &Profile{
		SampleType: []*ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Function:   []*Function{sf},
		Mapping:    []*Mapping{ms},
		Location: []*Location{{
			ID: 7, Address: 0x4500, Mapping: ms,
			Line: []Line{{Function: sf, Line: 42, Column: 8}},
		}},
	}

	res, err := CompleteSymbols(target, source)
	if err != nil {
		t.Fatal(err)
	}
	if got := locIDs(res.Completed); !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("completed = %v, want [1]", got)
	}
	ln := res.Profile.Location[0].Line[0]
	if ln.Function.Name != "fn" || ln.Function.SystemName != "fn.sys" {
		t.Errorf("existing fields changed: %+v", ln.Function)
	}
	if ln.Function.Filename != "dir/fn.go" || ln.Function.StartLine != 3 {
		t.Errorf("missing function fields not filled: %+v", ln.Function)
	}
	if ln.Line != 42 || ln.Column != 8 {
		t.Errorf("line/column not filled: %+v", ln)
	}
}

// TestRecomputeMappingFlags verifies flags derive from real per-address
// coverage rather than being turned on wholesale.
func TestRecomputeMappingFlags(t *testing.T) {
	symbolized := func(m *Mapping, id uint64, addr uint64, name string, line int64) *Location {
		return &Location{
			ID:      id,
			Address: addr,
			Mapping: m,
			Line:    []Line{testLine(testFunc(id, name, "fun.c", 1), line)},
		}
	}

	t.Run("partially covered", func(t *testing.T) {
		m := testMapping(1, 0x1000, 0x2000, 0, buildIDMain)
		m.HasFunctions, m.HasFilenames, m.HasLineNumbers, m.HasInlineFrames = true, true, true, true
		p := &Profile{
			Mapping: []*Mapping{m},
			Location: []*Location{
				symbolized(m, 1, 0x1100, "fun", 10),
				{ID: 2, Address: 0x1200, Mapping: m}, // unsymbolized
				{
					ID:      3,
					Address: 0x1300,
					Mapping: m,
					Line: []Line{
						testLine(testFunc(30, "inner", "fun.c", 1), 20),
						testLine(testFunc(31, "outer", "fun.c", 1), 30),
					},
				},
			},
		}
		recomputeMappingFlags(p)
		if m.HasFunctions || m.HasFilenames || m.HasLineNumbers {
			t.Errorf("partial coverage marked complete: %+v", m)
		}
		if !m.HasInlineFrames {
			t.Errorf("inline frames present but flag false")
		}
	})

	t.Run("fully covered", func(t *testing.T) {
		m := testMapping(1, 0x1000, 0x2000, 0, buildIDMain)
		p := &Profile{
			Mapping: []*Mapping{m},
			Location: []*Location{
				symbolized(m, 1, 0x1100, "fun1", 10),
				symbolized(m, 2, 0x1200, "fun2", 20),
			},
		}
		recomputeMappingFlags(p)
		if !(m.HasFunctions && m.HasFilenames && m.HasLineNumbers) {
			t.Errorf("fully covered mapping not marked as such: %+v", m)
		}
		if m.HasInlineFrames {
			t.Errorf("no inline frames but flag true")
		}
	})

	t.Run("empty mapping", func(t *testing.T) {
		m := testMapping(9, 0, 0, 0, buildIDLib)
		m.HasFunctions = true
		recomputeMappingFlags(&Profile{Mapping: []*Mapping{m}})
		if m.HasFunctions || m.HasFilenames || m.HasLineNumbers || m.HasInlineFrames {
			t.Errorf("mapping without locations should have no flags: %+v", m)
		}
	})
}
