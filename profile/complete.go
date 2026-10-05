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
)

// SymbolConflict describes a target location whose symbol information
// could not be copied from symbolized source profiles.
type SymbolConflict struct {
	// Location is the conflicting location in the completed profile.
	Location *Location
	// Field is the symbol field on which the disagreement was found.
	Field SymbolField
	// Frame is the index within Location.Line of the offending frame.
	Frame int
	// Existing is the value already present on the target location.
	Existing string
	// Candidates are the distinct non-empty values offered by the
	// source profiles, including Existing when it disagrees with them.
	Candidates []string
}

// SymbolField identifies a field that participates in symbolization.
type SymbolField int

const (
	// FunctionName is Function.Name.
	FunctionName SymbolField = iota
	// FunctionSystemName is Function.SystemName.
	FunctionSystemName
	// FunctionFilename is Function.Filename.
	FunctionFilename
	// FunctionStartLine is Function.StartLine.
	FunctionStartLine
	// LineNumber is the line number recorded on a Line.
	LineNumber
	// ColumnNumber is the column number recorded on a Line.
	ColumnNumber
	// InlineChainStructure is the shape (length and order) of the
	// inline chain.
	InlineChainStructure
)

func (f SymbolField) String() string {
	switch f {
	case FunctionName:
		return "function name"
	case FunctionSystemName:
		return "function system name"
	case FunctionFilename:
		return "file name"
	case FunctionStartLine:
		return "function start line"
	case LineNumber:
		return "line number"
	case ColumnNumber:
		return "column number"
	case InlineChainStructure:
		return "inline chain structure"
	default:
		return "unknown"
	}
}

// SymbolCompletion reports the outcome of CompleteSymbols.
type SymbolCompletion struct {
	// Profile is the completed profile. It is independent of both the
	// target and the source profiles; editing it never mutates them.
	Profile *Profile
	// Completed lists target locations that gained at least one missing
	// symbol field.
	Completed []*Location
	// Unmatched lists target locations for which no source location with
	// the same build ID and file-relative address was found.
	Unmatched []*Location
	// Conflicts lists locations left untouched because a non-empty value
	// already on the target disagreed with a source value, or because
	// multiple sources offered different values.
	Conflicts []SymbolConflict
}

// CompleteSymbols fills missing symbol information in a copy of the
// target profile using already-symbolized source profiles. Locations
// are matched by non-empty mapping build ID and file-relative address
// (address - mapping start + mapping offset), so neither profile
// location/function IDs nor process load bases need to agree.
//
// Symbols are only copied frame by frame when the target location has
// an empty inline chain or an inline chain of the same length as the
// source. Existing non-empty fields are never overwritten: when they
// disagree with a source value, or when different sources offer
// different values, the location is recorded as a conflict and is
// left unchanged. Addresses, sample values and labels are preserved.
//
// No external symbolization (addr2line, binaries or symbol servers) is
// performed.
func CompleteSymbols(target *Profile, sources ...*Profile) (*SymbolCompletion, error) {
	if target == nil {
		return nil, fmt.Errorf("complete symbols: nil target profile")
	}

	// Index source locations by build ID and file-relative address.
	type srcKey struct {
		buildID string
		address uint64
	}
	sourceLocs := map[srcKey][]*Location{}
	for _, src := range sources {
		if src == nil {
			continue
		}
		for _, l := range src.Location {
			if l == nil || l.Mapping == nil || l.Mapping.BuildID == "" {
				continue
			}
			k := srcKey{l.Mapping.BuildID, fileRelativeAddress(l)}
			sourceLocs[k] = append(sourceLocs[k], l)
		}
	}

	completed := target.Copy()

	// Reuse a single Function per distinct set of contents within the
	// completed profile, both for pre-existing and imported functions.
	funcs := map[functionKey]*Function{}
	for _, f := range completed.Function {
		funcs[f.key()] = f
	}
	intern := func(f *Function) *Function {
		if f == nil {
			return nil
		}
		k := f.key()
		if existing, ok := funcs[k]; ok {
			return existing
		}
		f.ID = uint64(len(completed.Function) + 1)
		completed.Function = append(completed.Function, f)
		funcs[k] = f
		return f
	}

	result := &SymbolCompletion{Profile: completed}

	for _, dst := range completed.Location {
		if dst == nil || dst.Mapping == nil || dst.Mapping.BuildID == "" {
			result.Unmatched = append(result.Unmatched, dst)
			continue
		}
		candidates := sourceLocs[srcKey{dst.Mapping.BuildID, fileRelativeAddress(dst)}]
		if len(candidates) == 0 {
			result.Unmatched = append(result.Unmatched, dst)
			continue
		}

		status, conflicts := processLocation(dst, candidates, intern)
		result.Conflicts = append(result.Conflicts, conflicts...)
		switch status {
		case locCompleted:
			result.Completed = append(result.Completed, dst)
		case locNeedsData:
			result.Unmatched = append(result.Unmatched, dst)
		}
	}

	recomputeMappingFlags(completed)

	sortLocations(result.Completed)
	sortLocations(result.Unmatched)
	sort.SliceStable(result.Conflicts, func(i, j int) bool {
		return result.Conflicts[i].Location.ID < result.Conflicts[j].Location.ID
	})

	if err := completed.CheckValid(); err != nil {
		return nil, fmt.Errorf("complete symbols: %v", err)
	}
	return result, nil
}

// fileRelativeAddress returns the address of a location relative to
// the start of its backing binary file, independent of the load base.
func fileRelativeAddress(l *Location) uint64 {
	if l == nil || l.Mapping == nil {
		return 0
	}
	return l.Address - l.Mapping.Start + l.Mapping.Offset
}

type locStatus int

const (
	locOK locStatus = iota
	locCompleted
	locNeedsData
	locConflict
)

// processLocation decides, for one target location and the source
// locations sharing its key, whether symbols can be copied. The target
// is modified only after every candidate has been checked, so the
// outcome is independent of the source order. It returns the target's
// status and any conflicts found for it.
func processLocation(dst *Location, candidates []*Location, intern func(*Function) *Function) (locStatus, []SymbolConflict) {
	var usable []*Location
	chainLens := map[int]bool{}
	for _, c := range candidates {
		if len(c.Line) == 0 {
			continue
		}
		chainLens[len(c.Line)] = true
		if len(dst.Line) == 0 || len(c.Line) == len(dst.Line) {
			usable = append(usable, c)
		}
	}

	if len(chainLens) == 0 {
		// No candidate carries symbol information. A target with no
		// inline chain of its own remains unmatched; a target that
		// already has frames is left untouched and not reported.
		if len(dst.Line) == 0 {
			return locNeedsData, nil
		}
		return locOK, nil
	}

	var conflicts []SymbolConflict
	switch {
	case len(dst.Line) == 0 && len(chainLens) > 1:
		// The target has no chain to preserve, but the sources disagree
		// on its shape; choosing one would depend on input order.
		conflicts = append(conflicts, SymbolConflict{
			Location:   dst,
			Field:      InlineChainStructure,
			Candidates: intKeysSorted(chainLens),
		})
		return locConflict, conflicts
	case len(dst.Line) != 0 && !chainLens[len(dst.Line)]:
		// Every symbolized source has an inline chain of a different
		// length, so the chains' structures do not match.
		conflicts = append(conflicts, SymbolConflict{
			Location:   dst,
			Field:      InlineChainStructure,
			Existing:   fmt.Sprintf("%d", len(dst.Line)),
			Candidates: intKeysSorted(chainLens),
		})
		return locConflict, conflicts
	}

	frames := len(usable[0].Line)
	fillable := len(dst.Line) == 0

	if len(dst.Line) != 0 && len(chainLens) > 1 {
		// Some sources agree with the target's structure while others
		// have chains of a different length; that disagreement is a
		// conflict even though a fill would otherwise be possible.
		conflicts = append(conflicts, SymbolConflict{
			Location:   dst,
			Field:      InlineChainStructure,
			Existing:   fmt.Sprintf("%d", len(dst.Line)),
			Candidates: intKeysSorted(chainLens),
		})
	}

	for i := 0; i < frames; i++ {
		var dstLine *Line
		if i < len(dst.Line) {
			dstLine = &dst.Line[i]
		}
		for _, g := range frameFields {
			var existing string
			sourceHasValue := false
			values := make([]string, 0, len(usable))
			for _, c := range usable {
				if v, ok := g.get(c.Line[i]); ok {
					values = append(values, v)
					sourceHasValue = true
				}
			}
			if dstLine != nil {
				if v, ok := g.get(*dstLine); ok {
					existing = v
					values = append(values, v)
				} else if sourceHasValue {
					fillable = true
				}
			} else if sourceHasValue {
				fillable = true
			}
			if c := buildConflict(dst, i, g.field, existing, values); c != nil {
				conflicts = append(conflicts, *c)
			}
		}
	}

	if len(conflicts) > 0 {
		return locConflict, conflicts
	}
	if !fillable {
		return locOK, nil
	}
	fillLocation(dst, usable[0], intern)
	return locCompleted, nil
}

func intKeysSorted(m map[int]bool) []string {
	var keys []int
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = fmt.Sprintf("%d", k)
	}
	return out
}

// frameField describes one symbol field of a single inline frame.
type frameField struct {
	field SymbolField
	// get returns the field's value and reports whether it is set
	// (non-empty/non-zero).
	get func(Line) (string, bool)
}

var frameFields = []frameField{
	{FunctionName, func(l Line) (string, bool) {
		if l.Function == nil || l.Function.Name == "" {
			return "", false
		}
		return l.Function.Name, true
	}},
	{FunctionSystemName, func(l Line) (string, bool) {
		if l.Function == nil || l.Function.SystemName == "" {
			return "", false
		}
		return l.Function.SystemName, true
	}},
	{FunctionFilename, func(l Line) (string, bool) {
		if l.Function == nil || l.Function.Filename == "" {
			return "", false
		}
		return l.Function.Filename, true
	}},
	{FunctionStartLine, func(l Line) (string, bool) {
		if l.Function == nil || l.Function.StartLine == 0 {
			return "", false
		}
		return fmt.Sprintf("%d", l.Function.StartLine), true
	}},
	{LineNumber, func(l Line) (string, bool) {
		if l.Line == 0 {
			return "", false
		}
		return fmt.Sprintf("%d", l.Line), true
	}},
	{ColumnNumber, func(l Line) (string, bool) {
		if l.Column == 0 {
			return "", false
		}
		return fmt.Sprintf("%d", l.Column), true
	}},
}

// buildConflict reports a field disagreement between the target and the
// distinct non-empty source values. A single offered value is not a
// conflict even if the target lacks the field.
func buildConflict(dst *Location, frame int, field SymbolField, existing string, values []string) *SymbolConflict {
	distinct := distinctSorted(values)
	if len(distinct) <= 1 {
		return nil
	}
	return &SymbolConflict{
		Location:   dst,
		Field:      field,
		Frame:      frame,
		Existing:   existing,
		Candidates: distinct,
	}
}

func distinctSorted(values []string) []string {
	seen := map[string]bool{}
	var distinct []string
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		distinct = append(distinct, v)
	}
	sort.Strings(distinct)
	return distinct
}

// fillLocation copies missing fields from src into dst, preserving the
// inline chain order and never overwriting non-empty fields.
func fillLocation(dst, src *Location, intern func(*Function) *Function) {
	importLine := func(sl Line) Line {
		f := &Function{}
		if sl.Function != nil {
			f.Name = sl.Function.Name
			f.SystemName = sl.Function.SystemName
			f.Filename = sl.Function.Filename
			f.StartLine = sl.Function.StartLine
		}
		return Line{Function: intern(f), Line: sl.Line, Column: sl.Column}
	}

	if len(dst.Line) == 0 {
		dst.Line = make([]Line, len(src.Line))
		for i := range src.Line {
			dst.Line[i] = importLine(src.Line[i])
		}
		return
	}

	for i := range dst.Line {
		sl := src.Line[i]
		dl := &dst.Line[i]
		f := &Function{}
		if dl.Function != nil {
			f.Name = dl.Function.Name
			f.SystemName = dl.Function.SystemName
			f.Filename = dl.Function.Filename
			f.StartLine = dl.Function.StartLine
		}
		if sl.Function != nil {
			if f.Name == "" {
				f.Name = sl.Function.Name
			}
			if f.SystemName == "" {
				f.SystemName = sl.Function.SystemName
			}
			if f.Filename == "" {
				f.Filename = sl.Function.Filename
			}
			if f.StartLine == 0 {
				f.StartLine = sl.Function.StartLine
			}
		}
		dl.Function = intern(f)
		if dl.Line == 0 {
			dl.Line = sl.Line
		}
		if dl.Column == 0 {
			dl.Column = sl.Column
		}
	}
}

// recomputeMappingFlags derives the mapping symbolization flags from
// the symbol data actually attached to each mapping's locations,
// rather than assuming a mapping is fully covered just because one
// address was completed. The function/filename/line flags require
// every location in the mapping to carry that information; the inline
// frame flag follows the profile.proto convention of being set when
// inline frames are present anywhere in the mapping.
func recomputeMappingFlags(p *Profile) {
	type coverage struct {
		allFunctions bool
		allFilenames bool
		allLines     bool
		hasInlines   bool
		hasLocation  bool
	}
	byMapping := map[*Mapping]*coverage{}
	for _, m := range p.Mapping {
		m.HasFunctions = false
		m.HasFilenames = false
		m.HasLineNumbers = false
		m.HasInlineFrames = false
		byMapping[m] = &coverage{allFunctions: true, allFilenames: true, allLines: true}
	}
	for _, l := range p.Location {
		m := l.Mapping
		if m == nil {
			continue
		}
		cov := byMapping[m]
		if cov == nil {
			continue
		}
		cov.hasLocation = true
		if len(l.Line) > 1 {
			cov.hasInlines = true
		}
		if len(l.Line) == 0 {
			cov.allFunctions = false
			cov.allFilenames = false
			cov.allLines = false
			continue
		}
		hasFunction := false
		hasFilename := false
		hasLine := false
		for _, ln := range l.Line {
			if ln.Function != nil && ln.Function.Name != "" {
				hasFunction = true
			}
			if ln.Function != nil && ln.Function.Filename != "" {
				hasFilename = true
			}
			if ln.Line != 0 {
				hasLine = true
			}
		}
		cov.allFunctions = cov.allFunctions && hasFunction
		cov.allFilenames = cov.allFilenames && hasFilename
		cov.allLines = cov.allLines && hasLine
	}
	for m, cov := range byMapping {
		m.HasFunctions = cov.hasLocation && cov.allFunctions
		m.HasFilenames = cov.hasLocation && cov.allFilenames
		m.HasLineNumbers = cov.hasLocation && cov.allLines
		m.HasInlineFrames = cov.hasInlines
	}
}

func sortLocations(locs []*Location) {
	sort.SliceStable(locs, func(i, j int) bool {
		return locs[i].ID < locs[j].ID
	})
}
