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
	"cmp"
	"fmt"
	"slices"
)

// MappingSegment describes one piece of a mapping relocation plan: the
// half-open source interval [Start, Limit) of the original mapping is moved
// to NewStart in the relocated profile.
type MappingSegment struct {
	Start    uint64 // Start of the source interval, inclusive.
	Limit    uint64 // End of the source interval, exclusive.
	NewStart uint64 // New load address for the start of the segment.
}

// RelocateMappings returns a new profile in which the mappings selected by
// plan (keyed by Mapping.ID) are split into the given segments and moved to
// the requested load addresses. Mappings not present in plan keep their
// original layout.
//
// The segments listed for a mapping must cover its whole [Start, Limit)
// range exactly once: no gaps, no overlaps, and no empty segments. For every
// segment a new Mapping is created with Start/Limit set to the new addresses
// and Offset adjusted by the segment's displacement within the original
// mapping, so the file-relative address (Address-Start+Offset) of every
// location is preserved. File, BuildID and the symbolization flags are
// carried over. The first segment (in source address order) keeps the
// original mapping ID; the remaining segments are assigned fresh IDs that do
// not conflict with any existing mapping ID.
//
// Locations are assigned to segments based on their original address: an
// address on a segment boundary belongs to the later segment. Sample, Line
// and Function references are preserved.
//
// The whole relocation is rejected, leaving the receiver unmodified, if any
// segment endpoint or file offset computation overflows, if any resulting
// non-empty address interval overlaps another, or if a location of a
// relocated mapping falls outside the mapping's original address range.
func (p *Profile) RelocateMappings(plan map[uint64][]MappingSegment) (*Profile, error) {
	// Phase 1: validate the plan against the receiver, read-only.
	mappingsByID := make(map[uint64]*Mapping, len(p.Mapping))
	for _, m := range p.Mapping {
		mappingsByID[m.ID] = m
	}
	segmentsByMapping := make(map[*Mapping][]MappingSegment, len(plan))
	for id, segs := range plan {
		m := mappingsByID[id]
		if m == nil {
			return nil, fmt.Errorf("no mapping with ID %d", id)
		}
		segs = slices.Clone(segs)
		slices.SortFunc(segs, func(a, b MappingSegment) int { return cmp.Compare(a.Start, b.Start) })
		next := m.Start
		for _, s := range segs {
			if s.Start >= s.Limit {
				return nil, fmt.Errorf("mapping %d: empty segment [%#x, %#x)", m.ID, s.Start, s.Limit)
			}
			if s.Start != next {
				return nil, fmt.Errorf("mapping %d: segments do not cover [%#x, %#x) exactly: gap or overlap at %#x", m.ID, m.Start, m.Limit, s.Start)
			}
			size := s.Limit - s.Start
			if s.NewStart+size < s.NewStart {
				return nil, fmt.Errorf("mapping %d: segment [%#x, %#x) relocated to %#x overflows the address space", m.ID, s.Start, s.Limit, s.NewStart)
			}
			if displacement := s.Start - m.Start; m.Offset+displacement < m.Offset {
				return nil, fmt.Errorf("mapping %d: file offset of segment [%#x, %#x) overflows", m.ID, s.Start, s.Limit)
			}
			next = s.Limit
		}
		if next != m.Limit {
			return nil, fmt.Errorf("mapping %d: segments do not cover [%#x, %#x) exactly: coverage ends at %#x", m.ID, m.Start, m.Limit, next)
		}
		segmentsByMapping[m] = segs
	}

	// Check that the resulting non-empty address intervals do not overlap.
	type interval struct{ start, limit uint64 }
	intervals := make([]interval, 0, len(p.Mapping)+len(plan))
	for _, m := range p.Mapping {
		if segs, ok := segmentsByMapping[m]; ok {
			for _, s := range segs {
				intervals = append(intervals, interval{s.NewStart, s.NewStart + (s.Limit - s.Start)})
			}
		} else if m.Start < m.Limit {
			intervals = append(intervals, interval{m.Start, m.Limit})
		}
	}
	slices.SortFunc(intervals, func(a, b interval) int { return cmp.Compare(a.start, b.start) })
	for i := 1; i < len(intervals); i++ {
		if intervals[i].start < intervals[i-1].limit {
			return nil, fmt.Errorf("relocated address ranges [%#x, %#x) and [%#x, %#x) overlap",
				intervals[i-1].start, intervals[i-1].limit, intervals[i].start, intervals[i].limit)
		}
	}

	// Check that locations of relocated mappings stay within bounds.
	for _, l := range p.Location {
		m := l.Mapping
		if m == nil {
			continue
		}
		if _, ok := segmentsByMapping[m]; !ok {
			continue
		}
		if l.Address < m.Start || l.Address >= m.Limit {
			return nil, fmt.Errorf("location %d address %#x is outside mapping %d range [%#x, %#x)",
				l.ID, l.Address, m.ID, m.Start, m.Limit)
		}
	}

	// Phase 2: apply the plan to a copy of the profile.
	cp := p.Copy()

	var maxID uint64
	for _, m := range cp.Mapping {
		maxID = max(maxID, m.ID)
	}
	nextID := maxID + 1

	type relocation struct {
		seg MappingSegment
		m   *Mapping
	}
	relocations := make(map[*Mapping][]relocation, len(segmentsByMapping))
	relocated := make(map[uint64][]MappingSegment, len(segmentsByMapping))
	for m, segs := range segmentsByMapping {
		relocated[m.ID] = segs
	}

	newMappings := make([]*Mapping, 0, len(cp.Mapping)+len(relocated))
	for _, m := range cp.Mapping {
		segs, ok := relocated[m.ID]
		if !ok {
			newMappings = append(newMappings, m)
			continue
		}
		for i, s := range segs {
			nm := *m
			nm.Start = s.NewStart
			nm.Limit = s.NewStart + (s.Limit - s.Start)
			nm.Offset = m.Offset + (s.Start - m.Start)
			if i > 0 {
				nm.ID = nextID
				nextID++
			}
			segMapping := &nm
			relocations[m] = append(relocations[m], relocation{seg: s, m: segMapping})
			newMappings = append(newMappings, segMapping)
		}
	}
	cp.Mapping = newMappings

	for _, l := range cp.Location {
		rels, ok := relocations[l.Mapping]
		if !ok {
			continue
		}
		for _, r := range rels {
			if l.Address >= r.seg.Start && l.Address < r.seg.Limit {
				l.Address = r.m.Start + (l.Address - r.seg.Start)
				l.Mapping = r.m
				break
			}
		}
	}

	return cp, nil
}
