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

// Implements methods to partition samples from profiles into
// independent profiles by entry point.

package profile

import "regexp"

// PartitionEntry describes one output partition of Profile.Partition:
// samples whose root-most matching logical frame matches Match are
// attributed to the entry named Name.
type PartitionEntry struct {
	// Name identifies the entry, e.g. a business entry point.
	Name string
	// Match is applied to function names and file names of logical
	// frames. A nil Match never matches.
	Match *regexp.Regexp
}

// partitionHit records how one sample was attributed: the partition
// group it belongs to and, for matched samples, the logical frame at
// which the output stack starts.
type partitionHit struct {
	sample *Sample
	loc    int // index into sample.Location of the hit location
	line   int // index into Location.Line of the hit line, -1 if unmatched
}

// Partition attributes every sample of p to the first entry point its
// logical stack enters, and returns one independent profile per entry
// (in the order entries are given) plus a profile with the samples
// that matched no entry.
//
// The logical stack of a sample is scanned from the root towards the
// leaf, expanding inlined frames: locations are visited from
// sample.Location[len-1] (root) down to sample.Location[0] (leaf), and
// within a location lines are visited from Line[len-1] (outermost
// inlined caller) down to Line[0] (leaf function). The scan stops at
// the first frame matching any entry, so a sample is attributed to the
// entry closest to the root and recursive re-entry into the same entry
// is not counted again. If a single frame matches several entries, the
// first entry in the given order wins. Every sample lands in exactly
// one output.
//
// The stack kept for a matched sample starts at the hit frame and
// extends to the leaf. The cut may fall in the middle of the inlined
// lines of a location; such locations are copied rather than modified
// in place, so p is never mutated. Samples in the unmatched profile
// keep their complete stacks. Sample values and all labels are carried
// over unchanged.
//
// Each output profile only contains the mappings, locations and
// functions it references, with fresh IDs assigned, and shares no
// mutable state with p or the other outputs.
func (p *Profile) Partition(entries []PartitionEntry) (partitions []*Profile, unmatched *Profile) {
	// groups[i] holds the hits for entries[i]; the last group holds
	// samples that matched no entry.
	groups := make([][]partitionHit, len(entries)+1)
	for _, s := range p.Sample {
		h := partitionHit{sample: s, line: -1}
		group := len(entries)
	scan:
		// Visit logical frames from the root to the leaf.
		for li := len(s.Location) - 1; li >= 0; li-- {
			loc := s.Location[li]
			for ln := len(loc.Line) - 1; ln >= 0; ln-- {
				if g, ok := matchPartitionFrame(entries, loc.Line[ln].Function); ok {
					group, h.loc, h.line = g, li, ln
					break scan
				}
			}
		}
		groups[group] = append(groups[group], h)
	}

	partitions = make([]*Profile, len(entries))
	for i := range entries {
		partitions[i] = p.buildPartition(groups[i])
	}
	return partitions, p.buildPartition(groups[len(entries)])
}

// matchPartitionFrame returns the index of the first entry matching
// the given function, or false if no entry matches.
func matchPartitionFrame(entries []PartitionEntry, fn *Function) (int, bool) {
	if fn == nil {
		return 0, false
	}
	for i, e := range entries {
		if e.Match != nil && (e.Match.MatchString(fn.Name) || e.Match.MatchString(fn.Filename)) {
			return i, true
		}
	}
	return 0, false
}

// buildPartition assembles an independent profile from the given hits.
// A hit with line >= 0 keeps the stack from the hit frame to the leaf;
// a hit with line == -1 keeps the complete stack.
func (p *Profile) buildPartition(hits []partitionHit) *Profile {
	np := &Profile{
		SampleType:        make([]*ValueType, len(p.SampleType)),
		DefaultSampleType: p.DefaultSampleType,
		Period:            p.Period,
		TimeNanos:         p.TimeNanos,
		DurationNanos:     p.DurationNanos,
		Comments:          append([]string(nil), p.Comments...),
		DocURL:            p.DocURL,
		DropFrames:        p.DropFrames,
		KeepFrames:        p.KeepFrames,
	}
	for i, vt := range p.SampleType {
		vtCopy := *vt
		np.SampleType[i] = &vtCopy
	}
	if p.PeriodType != nil {
		ptCopy := *p.PeriodType
		np.PeriodType = &ptCopy
	}

	c := &partitionCopier{
		p:         np,
		mappings:  make(map[*Mapping]*Mapping),
		functions: make(map[*Function]*Function),
		locations: make(map[partitionLocationKey]*Location),
	}
	for _, h := range hits {
		s := h.sample
		ns := &Sample{
			Value:    append([]int64(nil), s.Value...),
			Label:    copyStringLabels(s.Label),
			NumLabel: copyNumLabels(s.NumLabel),
			NumUnit:  copyStringLabels(s.NumUnit),
		}
		// Sample locations are stored leaf-first, so the kept stack is
		// the prefix up to and including the hit location.
		last := len(s.Location) - 1
		if h.line >= 0 {
			last = h.loc
		}
		for li := 0; li <= last; li++ {
			trim := len(s.Location[li].Line)
			if li == last && h.line >= 0 {
				// The cut may fall in the middle of the inlined
				// lines of the hit location.
				trim = h.line + 1
			}
			ns.Location = append(ns.Location, c.location(s.Location[li], trim))
		}
		np.Sample = append(np.Sample, ns)
	}
	return np
}

// partitionLocationKey identifies a location copy within an output
// profile. The same source location may be copied with different
// numbers of kept lines, e.g. when it is the hit location for one
// sample but sits below the hit frame for another.
type partitionLocationKey struct {
	loc  *Location
	trim int
}

// partitionCopier deep-copies mappings, functions and locations into
// one output profile, deduplicating shared objects and assigning fresh
// sequential IDs.
type partitionCopier struct {
	p         *Profile
	mappings  map[*Mapping]*Mapping
	functions map[*Function]*Function
	locations map[partitionLocationKey]*Location
}

func (c *partitionCopier) mapping(m *Mapping) *Mapping {
	if m == nil {
		return nil
	}
	if nm, ok := c.mappings[m]; ok {
		return nm
	}
	nm := *m
	nm.ID = uint64(len(c.p.Mapping)) + 1
	c.p.Mapping = append(c.p.Mapping, &nm)
	c.mappings[m] = &nm
	return &nm
}

func (c *partitionCopier) function(f *Function) *Function {
	if f == nil {
		return nil
	}
	if nf, ok := c.functions[f]; ok {
		return nf
	}
	nf := *f
	nf.ID = uint64(len(c.p.Function)) + 1
	c.p.Function = append(c.p.Function, &nf)
	c.functions[f] = &nf
	return &nf
}

// location returns the copy of l keeping its first trim lines (Line is
// stored leaf-first, so trimming drops outermost inlined callers).
func (c *partitionCopier) location(l *Location, trim int) *Location {
	key := partitionLocationKey{loc: l, trim: trim}
	if nl, ok := c.locations[key]; ok {
		return nl
	}
	nl := &Location{
		ID:       uint64(len(c.p.Location)) + 1,
		Mapping:  c.mapping(l.Mapping),
		Address:  l.Address,
		IsFolded: l.IsFolded,
		Line:     make([]Line, trim),
	}
	for i := 0; i < trim; i++ {
		nl.Line[i] = Line{
			Function: c.function(l.Line[i].Function),
			Line:     l.Line[i].Line,
			Column:   l.Line[i].Column,
		}
	}
	c.p.Location = append(c.p.Location, nl)
	c.locations[key] = nl
	return nl
}

func copyStringLabels(src map[string][]string) map[string][]string {
	if src == nil {
		return nil
	}
	dst := make(map[string][]string, len(src))
	for k, v := range src {
		dst[k] = append([]string(nil), v...)
	}
	return dst
}

func copyNumLabels(src map[string][]int64) map[string][]int64 {
	if src == nil {
		return nil
	}
	dst := make(map[string][]int64, len(src))
	for k, v := range src {
		dst[k] = append([]int64(nil), v...)
	}
	return dst
}
