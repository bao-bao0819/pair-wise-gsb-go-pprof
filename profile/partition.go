// Implements methods to partition profile samples by entry point.

package profile

import (
	"regexp"
	"slices"
)

// PartitionRule names a logical entry point and gives the regular
// expression that identifies its frames. Rules are evaluated in the
// order they are provided: when a single logical frame matches several
// rules, the first rule wins.
type PartitionRule struct {
	// Name identifies the partition in the returned map. Names are
	// expected to be unique; a duplicate name overwrites the earlier
	// partition.
	Name string
	// Re is matched against the function names of the logical frames,
	// including inlined frames. A nil Re never matches.
	Re *regexp.Regexp
}

// Partition splits the samples of p into disjoint per-entry profiles
// according to rules, plus a profile holding the samples that matched
// no rule.
//
// For every sample the logical stack is scanned from the root towards
// the leaf, expanding inlined frames, and the first frame matching any
// rule assigns the whole sample to that rule's partition. The scan then
// stops, so a sample is attributed to exactly one partition and
// recursive re-entry into the same entry point is not counted twice.
// Samples matching no rule keep their full stack in the returned
// unmatched profile.
//
// The stack kept in a partition starts at the matched frame and
// continues to the leaf: frames closer to the root than the matched
// entry point are dropped. The cut may land in the middle of the
// inlined frames of a location; the shared input location is never
// modified. Sample values and all labels are carried over unchanged.
//
// Each returned profile is compacted independently: only referenced
// mappings, locations and functions are kept, IDs are renumbered, and
// all objects are copies, so mutating an output never affects the
// input or the other outputs.
func (p *Profile) Partition(rules []PartitionRule) (map[string]*Profile, *Profile) {
	parts := make([]*Profile, len(rules))
	for i := range parts {
		parts[i] = p.partitionHeader()
	}
	unmatched := p.partitionHeader()

	// trimmed caches locations cut in the middle of their inlined
	// frames, so identical cuts share a single copy.
	trimmed := map[trimKey]*Location{}

	for _, s := range p.Sample {
		dst, locs := unmatched, s.Location
		if i, j, r := partitionHit(s.Location, rules); r >= 0 {
			dst = parts[r]
			locs = trimStack(s.Location, i, j, trimmed)
		}
		dst.Sample = append(dst.Sample, &Sample{
			Location: locs,
			Value:    s.Value,
			Label:    s.Label,
			NumLabel: s.NumLabel,
			NumUnit:  s.NumUnit,
		})
	}

	out := make(map[string]*Profile, len(rules))
	for i, part := range parts {
		part.finalizePartition()
		out[rules[i].Name] = part
	}
	unmatched.finalizePartition()
	return out, unmatched
}

// partitionHeader returns a profile carrying the header fields of p
// (everything but the samples and the object tables).
func (p *Profile) partitionHeader() *Profile {
	h := &Profile{
		DefaultSampleType: p.DefaultSampleType,
		Comments:          slices.Clone(p.Comments),
		DocURL:            p.DocURL,
		DropFrames:        p.DropFrames,
		KeepFrames:        p.KeepFrames,
		TimeNanos:         p.TimeNanos,
		DurationNanos:     p.DurationNanos,
		Period:            p.Period,
	}
	h.SampleType = make([]*ValueType, len(p.SampleType))
	for i, vt := range p.SampleType {
		cpy := *vt
		h.SampleType[i] = &cpy
	}
	if p.PeriodType != nil {
		pt := *p.PeriodType
		h.PeriodType = &pt
	}
	return h
}

// partitionHit scans the logical stack of locs from the root to the
// leaf and returns the location index, line index and rule index of
// the first frame matching any rule. It returns -1 as the rule index
// if no frame matches.
func partitionHit(locs []*Location, rules []PartitionRule) (locIdx, lineIdx, ruleIdx int) {
	// Sample locations are ordered leaf-first, so the root is the last
	// location. Within a location, inlined frames are ordered
	// innermost-first, so the outermost frame is the last line.
	for i := len(locs) - 1; i >= 0; i-- {
		lines := locs[i].Line
		for j := len(lines) - 1; j >= 0; j-- {
			fn := lines[j].Function
			if fn == nil {
				continue
			}
			for r := range rules {
				if rules[r].Re != nil && rules[r].Re.MatchString(fn.Name) {
					return i, j, r
				}
			}
		}
	}
	return 0, 0, -1
}

// trimKey identifies a location cut down to a given number of lines.
type trimKey struct {
	loc   *Location
	lines int
}

// trimStack returns the frames of locs from the hit frame (location i,
// line j) down to the leaf. The hit location is shallow-copied with
// its Line slice truncated when the cut lands in the middle of its
// inlined frames, so the shared input location is never modified.
func trimStack(locs []*Location, i, j int, trimmed map[trimKey]*Location) []*Location {
	out := make([]*Location, i+1)
	copy(out, locs[:i+1])
	if hit := locs[i]; j < len(hit.Line)-1 {
		key := trimKey{hit, j + 1}
		loc, ok := trimmed[key]
		if !ok {
			cpy := *hit
			cpy.Line = hit.Line[:j+1]
			loc = &cpy
			trimmed[key] = loc
		}
		out[i] = loc
	}
	return out
}

// finalizePartition rebuilds the mapping, location and function tables
// of a partition output in place, keeping only the objects referenced
// by its samples and assigning fresh sequential IDs. Every object is
// copied, so the result is fully independent of the input profile and
// of the other partition outputs.
func (p *Profile) finalizePartition() {
	mappings := map[*Mapping]*Mapping{}
	functions := map[*Function]*Function{}
	locations := map[*Location]*Location{}

	p.Mapping = nil
	p.Location = nil
	p.Function = nil

	for _, s := range p.Sample {
		s.Value = slices.Clone(s.Value)
		s.Label = cloneLabels(s.Label)
		s.NumLabel = cloneNumLabels(s.NumLabel)
		s.NumUnit = cloneLabels(s.NumUnit)
		for i, loc := range s.Location {
			nl, ok := locations[loc]
			if !ok {
				nl = &Location{
					ID:       uint64(len(p.Location) + 1),
					Address:  loc.Address,
					IsFolded: loc.IsFolded,
				}
				if loc.Mapping != nil {
					nm, ok := mappings[loc.Mapping]
					if !ok {
						cpy := *loc.Mapping
						nm = &cpy
						nm.ID = uint64(len(p.Mapping) + 1)
						mappings[loc.Mapping] = nm
						p.Mapping = append(p.Mapping, nm)
					}
					nl.Mapping = nm
				}
				nl.Line = make([]Line, len(loc.Line))
				for k, ln := range loc.Line {
					nl.Line[k] = Line{Line: ln.Line, Column: ln.Column}
					if fn := ln.Function; fn != nil {
						nf, ok := functions[fn]
						if !ok {
							cpy := *fn
							nf = &cpy
							nf.ID = uint64(len(p.Function) + 1)
							functions[fn] = nf
							p.Function = append(p.Function, nf)
						}
						nl.Line[k].Function = nf
					}
				}
				locations[loc] = nl
				p.Location = append(p.Location, nl)
			}
			s.Location[i] = nl
		}
	}
}

// cloneLabels deep-copies a string label map.
func cloneLabels(m map[string][]string) map[string][]string {
	if m == nil {
		return nil
	}
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = slices.Clone(v)
	}
	return out
}

// cloneNumLabels deep-copies a numeric label map.
func cloneNumLabels(m map[string][]int64) map[string][]int64 {
	if m == nil {
		return nil
	}
	out := make(map[string][]int64, len(m))
	for k, v := range m {
		out[k] = slices.Clone(v)
	}
	return out
}
