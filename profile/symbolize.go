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
	"sort"
	"strconv"
)

// SymbolField identifies a field that can be copied between two
// symbolized locations.
type SymbolField int

const (
	// FunctionName is the name of the function of an inline frame.
	FunctionName SymbolField = iota
	// FunctionSystemName is the system name of the function of an inline frame.
	FunctionSystemName
	// FunctionStartLine is the start line of the function of an inline frame.
	FunctionStartLine
	// Filename is the source file name of an inline frame.
	Filename
	// LineNumber is the source line number of an inline frame.
	LineNumber
	// ColumnNumber is the source column number of an inline frame.
	ColumnNumber
	// InlineChainLength is the number of frames in the location's inline chain.
	InlineChainLength
)

func (f SymbolField) String() string {
	switch f {
	case FunctionName:
		return "function name"
	case FunctionSystemName:
		return "function system name"
	case FunctionStartLine:
		return "function start line"
	case Filename:
		return "file name"
	case LineNumber:
		return "line number"
	case ColumnNumber:
		return "column number"
	case InlineChainLength:
		return "inline chain length"
	}
	return "unknown field"
}

// SymbolConflict describes why a location could not be completed:
// either the location already holds information that contradicts a
// source, or multiple sources provide different candidates for it. A
// conflicting location is never modified.
type SymbolConflict struct {
	// Location is the target location that could not be completed.
	Location *Location
	// Field is the conflicting field. It is InlineChainLength when the
	// source and target inline chains have different lengths.
	Field SymbolField
	// Frame is the 0-based index within the inline chain of the frame
	// with the conflict. It is -1 when Field is InlineChainLength.
	Frame int
	// Existing is the non-empty value already present in the target
	// location. It is empty when the target field was unset and the
	// sources disagreed, or for an inline chain length conflict on an
	// empty chain.
	Existing string
	// Candidates are the distinct, non-empty values that were found in
	// the sources (including Existing when it was non-empty), sorted.
	Candidates []string
}

// SymbolCompletionResult reports the outcome of CompleteSymbols.
type SymbolCompletionResult struct {
	// Completed lists target locations that had at least one field filled.
	Completed []*Location
	// Unmatched lists target locations that no source could fill, either
	// because they have no mapping with a BuildID or because no source
	// location matched their file-relative address.
	Unmatched []*Location
	// Conflicts lists target locations left untouched because of a
	// contradiction, see SymbolConflict.
	Conflicts []*SymbolConflict
}

// CompleteSymbols fills missing symbolization information in p using
// the already-symbolized profiles in sources.
//
// Locations are matched by the non-empty BuildID of their mapping and
// by their file-relative address (address - mapping start + mapping
// offset), so matching is independent of location IDs, function IDs,
// and the process load base. It never invokes addr2line, a browser or
// any external symbol service.
//
// A target location may be completed only when its inline chain is
// empty or has exactly the same length as the source chain; the order
// of the frames is preserved. Empty fields of the target are filled
// field by field. If a non-empty function name, file name or line
// number disagrees with a source, or if two sources offer different
// candidates, the location is left unchanged and a conflict is
// recorded. Imported functions get fresh IDs, and identical functions
// are reused instead of duplicated.
//
// Addresses, sample values and labels are not modified. The Has* flags
// of the mappings are recomputed from the actual information present
// after completion.
//
// The sources are never modified.
func (p *Profile) CompleteSymbols(sources ...*Profile) (*SymbolCompletionResult, error) {
	if p == nil {
		return nil, fmt.Errorf("cannot complete symbols of a nil profile")
	}

	candidatesByKey := map[symbolLocationKey][]symbolCandidate{}
	for _, src := range sources {
		if src == nil || src == p {
			continue
		}
		for _, loc := range src.Location {
			key, ok := symbolLocationKeyFor(loc)
			if !ok || len(loc.Line) == 0 || !hasSymbolInfo(loc.Line) {
				continue
			}
			candidatesByKey[key] = append(candidatesByKey[key], symbolCandidate{lines: loc.Line})
		}
	}

	filler := &symbolFiller{
		p:          p,
		functions:  make(map[functionKey]*Function, len(p.Function)),
		nextFuncID: 0,
		completed:  map[*Location]bool{},
	}
	for _, f := range p.Function {
		filler.functions[f.key()] = f
		if f.ID > filler.nextFuncID {
			filler.nextFuncID = f.ID
		}
	}

	result := &SymbolCompletionResult{}
	for _, loc := range p.Location {
		key, ok := symbolLocationKeyFor(loc)
		if !ok {
			result.Unmatched = append(result.Unmatched, loc)
			continue
		}
		cands := candidatesByKey[key]
		if len(cands) == 0 {
			result.Unmatched = append(result.Unmatched, loc)
			continue
		}

		// A different inline chain length is always a conflict: either
		// the sources disagree with each other, or a source disagrees
		// with the target, even when the target otherwise looks complete.
		if conflict := chainLengthConflict(loc, cands); conflict != nil {
			result.Conflicts = append(result.Conflicts, conflict)
			continue
		}

		// completeLocation first checks every non-empty target field
		// against the candidates (recording a contradiction even when the
		// location looks complete) and only then fills the empty fields.
		if conflict := filler.completeLocation(loc, cands); conflict != nil {
			result.Conflicts = append(result.Conflicts, conflict)
		} else if filler.completed[loc] {
			result.Completed = append(result.Completed, loc)
		}
	}

	filler.recomputeMappingFlags()

	sortLocations(result.Completed)
	sortLocations(result.Unmatched)
	sort.SliceStable(result.Conflicts, func(i, j int) bool {
		return result.Conflicts[i].Location.ID < result.Conflicts[j].Location.ID
	})
	return result, nil
}

type symbolLocationKey struct {
	buildID string
	address uint64
}

func symbolLocationKeyFor(loc *Location) (symbolLocationKey, bool) {
	m := loc.Mapping
	if m == nil || m.BuildID == "" || loc.Address < m.Start || loc.Address >= m.Limit {
		return symbolLocationKey{}, false
	}
	return symbolLocationKey{
		buildID: m.BuildID,
		address: loc.Address - m.Start + m.Offset,
	}, true
}

type symbolFiller struct {
	p          *Profile
	functions  map[functionKey]*Function
	nextFuncID uint64

	completed map[*Location]bool
}

type symbolCandidate struct {
	lines []Line
}

func chainLengthConflict(loc *Location, cands []symbolCandidate) *SymbolConflict {
	lengths := map[string]bool{}
	if len(loc.Line) != 0 {
		lengths[strconv.Itoa(len(loc.Line))] = true
	}
	for _, cand := range cands {
		lengths[strconv.Itoa(len(cand.lines))] = true
	}
	if len(lengths) <= 1 && len(loc.Line) != 0 {
		return nil
	}
	// With an empty target chain, conflicting candidates are a conflict;
	// a single agreed length is what the empty chain will adopt.
	if len(loc.Line) == 0 && len(lengths) == 1 {
		return nil
	}
	existing := ""
	if len(loc.Line) != 0 {
		existing = strconv.Itoa(len(loc.Line))
	}
	return &SymbolConflict{
		Location:   loc,
		Field:      InlineChainLength,
		Frame:      -1,
		Existing:   existing,
		Candidates: sortedValues(lengths),
	}
}

func (f *symbolFiller) completeLocation(loc *Location, cands []symbolCandidate) *SymbolConflict {
	// First pass: verify that all candidates are mutually consistent and
	// that none of them contradicts the non-empty core fields (function
	// name, file name and line number) of the target. Disagreement between
	// sources on any field is also a conflict.
	for frame := range cands[0].lines {
		for _, field := range symbolFields {
			candidateValues := map[string]bool{}
			for _, cand := range cands {
				if value, ok := frameFieldValue(cand.lines[frame], field); ok {
					candidateValues[value] = true
				}
			}
			if len(candidateValues) > 1 {
				return &SymbolConflict{
					Location:   loc,
					Field:      field,
					Frame:      frame,
					Existing:   "",
					Candidates: sortedValues(candidateValues),
				}
			}
			existing := ""
			if frame < len(loc.Line) {
				var hasExisting bool
				existing, hasExisting = frameFieldValue(loc.Line[frame], field)
				if hasExisting && isCoreSymbolField(field) {
					if value, ok := anyValue(candidateValues); ok && existing != value {
						return &SymbolConflict{
							Location:   loc,
							Field:      field,
							Frame:      frame,
							Existing:   existing,
							Candidates: sortedValues(candidateValues),
						}
					}
				}
			}
		}
	}

	// Second pass: fill the empty fields. The candidate values are
	// guaranteed to be consistent by the pass above.
	chainEmpty := len(loc.Line) == 0
	if chainEmpty {
		loc.Line = make([]Line, len(cands[0].lines))
	}
	for frame := range loc.Line {
		target := &loc.Line[frame]
		cand := cands[0].lines[frame]
		if target.Function == nil {
			target.Function = f.importFunction(cand.Function)
			f.completed[loc] = true
		} else if cand.Function != nil {
			fn := target.Function
			if fn.Name == "" && cand.Function.Name != "" {
				fn.Name = cand.Function.Name
				f.completed[loc] = true
			}
			if fn.SystemName == "" && cand.Function.SystemName != "" {
				fn.SystemName = cand.Function.SystemName
				f.completed[loc] = true
			}
			if fn.Filename == "" && cand.Function.Filename != "" {
				fn.Filename = cand.Function.Filename
				f.completed[loc] = true
			}
			if fn.StartLine == 0 && cand.Function.StartLine != 0 {
				fn.StartLine = cand.Function.StartLine
				f.completed[loc] = true
			}
		}
		if target.Line == 0 && cand.Line != 0 {
			target.Line = cand.Line
			f.completed[loc] = true
		}
		if target.Column == 0 && cand.Column != 0 {
			target.Column = cand.Column
			f.completed[loc] = true
		}
	}
	return nil
}

var symbolFields = []SymbolField{
	FunctionName,
	FunctionSystemName,
	FunctionStartLine,
	Filename,
	LineNumber,
	ColumnNumber,
}

// isCoreSymbolField reports whether a non-empty target value for the
// field contradicts a differing source value. Function name, file name
// and line number are the core fields named by the profile format; the
// other fields are simply preserved when already populated.
func isCoreSymbolField(field SymbolField) bool {
	switch field {
	case FunctionName, Filename, LineNumber:
		return true
	}
	return false
}

func anyValue(values map[string]bool) (string, bool) {
	for value := range values {
		return value, true
	}
	return "", false
}

func frameFieldValue(line Line, field SymbolField) (string, bool) {
	switch field {
	case FunctionName:
		if line.Function == nil || line.Function.Name == "" {
			return "", false
		}
		return line.Function.Name, true
	case FunctionSystemName:
		if line.Function == nil || line.Function.SystemName == "" {
			return "", false
		}
		return line.Function.SystemName, true
	case FunctionStartLine:
		if line.Function == nil || line.Function.StartLine == 0 {
			return "", false
		}
		return strconv.FormatInt(line.Function.StartLine, 10), true
	case Filename:
		if line.Function == nil || line.Function.Filename == "" {
			return "", false
		}
		return line.Function.Filename, true
	case LineNumber:
		if line.Line == 0 {
			return "", false
		}
		return strconv.FormatInt(line.Line, 10), true
	case ColumnNumber:
		if line.Column == 0 {
			return "", false
		}
		return strconv.FormatInt(line.Column, 10), true
	}
	return "", false
}

func (f *symbolFiller) importFunction(src *Function) *Function {
	if src == nil {
		return nil
	}
	if fn, ok := f.functions[src.key()]; ok {
		return fn
	}
	fn := &Function{
		ID:         f.nextFuncID + 1,
		Name:       src.Name,
		SystemName: src.SystemName,
		Filename:   src.Filename,
		StartLine:  src.StartLine,
	}
	f.nextFuncID = fn.ID
	f.functions[src.key()] = fn
	f.p.Function = append(f.p.Function, fn)
	return fn
}

func (f *symbolFiller) recomputeMappingFlags() {
	locsByMapping := map[*Mapping][]*Location{}
	for _, loc := range f.p.Location {
		if loc.Mapping != nil {
			locsByMapping[loc.Mapping] = append(locsByMapping[loc.Mapping], loc)
		}
	}
	for _, m := range f.p.Mapping {
		locs := locsByMapping[m]
		hasFunctions, hasFilenames, hasLineNumbers, hasInlineFrames := true, true, true, false
		if len(locs) == 0 {
			hasFunctions, hasFilenames, hasLineNumbers = false, false, false
		}
		for _, loc := range locs {
			if len(loc.Line) == 0 {
				hasFunctions, hasFilenames, hasLineNumbers = false, false, false
				continue
			}
			if len(loc.Line) > 1 {
				hasInlineFrames = true
			}
			for _, ln := range loc.Line {
				if ln.Function == nil || ln.Function.Name == "" {
					hasFunctions = false
				}
				if ln.Function == nil || ln.Function.Filename == "" {
					hasFilenames = false
				}
				if ln.Line == 0 {
					hasLineNumbers = false
				}
			}
		}
		m.HasFunctions = hasFunctions
		m.HasFilenames = hasFilenames
		m.HasLineNumbers = hasLineNumbers
		m.HasInlineFrames = hasInlineFrames
	}
}

func hasSymbolInfo(lines []Line) bool {
	for _, ln := range lines {
		if ln.Function != nil && (ln.Function.Name != "" || ln.Function.SystemName != "" ||
			ln.Function.Filename != "" || ln.Function.StartLine != 0) ||
			ln.Line != 0 || ln.Column != 0 {
			return true
		}
	}
	return false
}

func sortedValues(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func sortLocations(locs []*Location) {
	sort.SliceStable(locs, func(i, j int) bool {
		return locs[i].ID < locs[j].ID
	})
}
