package profile

import (
	"fmt"
	"math"
	"sort"
)

// MappingRelocation describes a destination for a half-open source interval
// of a mapping. Source intervals use the original mapping's address range,
// while NewStart is the address where that interval starts after relocation.
type MappingRelocation struct {
	MappingID uint64
	Start     uint64
	Limit     uint64
	NewStart  uint64
}

type relocationSegment struct {
	srcStart, srcLimit uint64
	dstStart, dstLimit uint64
	offset             uint64
	replacement        *Mapping
}

// RelocateMappings returns a copy of p with the selected mappings split and
// relocated. The source intervals for each selected mapping must partition
// that mapping without overlap or gaps. Unselected mappings retain their
// existing layout. Invalid relocations are rejected without modifying p.
func (p *Profile) RelocateMappings(relocations []MappingRelocation) (*Profile, error) {
	if err := p.CheckValid(); err != nil {
		return nil, err
	}

	mappingsByID := make(map[uint64]*Mapping, len(p.Mapping))
	maxMappingID := uint64(0)
	for _, m := range p.Mapping {
		mappingsByID[m.ID] = m
		if m.ID > maxMappingID {
			maxMappingID = m.ID
		}
		if m.Start > m.Limit {
			return nil, fmt.Errorf("mapping %d has invalid range [%#x, %#x)", m.ID, m.Start, m.Limit)
		}
	}

	segmentsByID := make(map[uint64][]*relocationSegment)
	for _, r := range relocations {
		m := mappingsByID[r.MappingID]
		if m == nil {
			return nil, fmt.Errorf("relocation references unknown mapping ID %d", r.MappingID)
		}
		if r.Start < m.Start || r.Limit > m.Limit || r.Start >= r.Limit {
			return nil, fmt.Errorf("relocation interval [%#x, %#x) is outside mapping %d range [%#x, %#x)", r.Start, r.Limit, r.MappingID, m.Start, m.Limit)
		}

		length := r.Limit - r.Start
		dstLimit, ok := addUint64(r.NewStart, length)
		if !ok {
			return nil, fmt.Errorf("relocation destination for mapping %d overflows: %#x + %#x", r.MappingID, r.NewStart, length)
		}
		displacement := r.Start - m.Start
		offset, ok := addUint64(m.Offset, displacement)
		if !ok {
			return nil, fmt.Errorf("relocation file offset for mapping %d overflows: %#x + %#x", r.MappingID, m.Offset, displacement)
		}
		if _, ok := addUint64(offset, length); !ok {
			return nil, fmt.Errorf("relocation file-offset endpoint for mapping %d overflows: %#x + %#x", r.MappingID, offset, length)
		}

		segmentsByID[r.MappingID] = append(segmentsByID[r.MappingID], &relocationSegment{
			srcStart: r.Start,
			srcLimit: r.Limit,
			dstStart: r.NewStart,
			dstLimit: dstLimit,
			offset:   offset,
		})
	}

	additionalIDs := uint64(0)
	for id, segments := range segmentsByID {
		sort.Slice(segments, func(i, j int) bool {
			return segments[i].srcStart < segments[j].srcStart
		})

		m := mappingsByID[id]
		next := m.Start
		for _, segment := range segments {
			if segment.srcStart != next {
				if segment.srcStart < next {
					return nil, fmt.Errorf("relocation intervals for mapping %d overlap", id)
				}
				return nil, fmt.Errorf("relocation intervals for mapping %d leave gap [%#x, %#x)", id, next, segment.srcStart)
			}
			next = segment.srcLimit
		}
		if next != m.Limit {
			return nil, fmt.Errorf("relocation intervals for mapping %d leave gap [%#x, %#x)", id, next, m.Limit)
		}

		if len(segments) > 1 {
			count, ok := addUint64(additionalIDs, uint64(len(segments)-1))
			if !ok {
				return nil, fmt.Errorf("relocation mapping ID allocation overflows")
			}
			additionalIDs = count
		}
	}
	if _, ok := addUint64(maxMappingID, additionalIDs); !ok {
		return nil, fmt.Errorf("relocation mapping ID allocation overflows")
	}

	for _, location := range p.Location {
		m := location.Mapping
		if m == nil {
			continue
		}
		if location.Address < m.Start || location.Address >= m.Limit {
			return nil, fmt.Errorf("location %d address %#x is outside mapping %d range [%#x, %#x)", location.ID, location.Address, m.ID, m.Start, m.Limit)
		}
		if _, ok := addUint64(m.Offset, location.Address-m.Start); !ok {
			return nil, fmt.Errorf("file-relative address for location %d overflows", location.ID)
		}
	}

	type addressInterval struct {
		start, limit uint64
	}
	var intervals []addressInterval
	for _, m := range p.Mapping {
		if segmentsByID[m.ID] == nil && m.Start < m.Limit {
			intervals = append(intervals, addressInterval{m.Start, m.Limit})
		}
	}
	for _, segments := range segmentsByID {
		for _, segment := range segments {
			intervals = append(intervals, addressInterval{segment.dstStart, segment.dstLimit})
		}
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].start != intervals[j].start {
			return intervals[i].start < intervals[j].start
		}
		return intervals[i].limit < intervals[j].limit
	})
	for i := 1; i < len(intervals); i++ {
		if intervals[i].start < intervals[i-1].limit {
			return nil, fmt.Errorf("relocated mappings overlap in [%#x, %#x)", intervals[i].start, min(intervals[i-1].limit, intervals[i].limit))
		}
	}

	relocated := p.Copy()
	nextID := maxMappingID
	segmentsByCopyID := make(map[uint64][]*relocationSegment, len(segmentsByID))
	replacements := make(map[uint64][]*Mapping, len(segmentsByID))
	for _, m := range relocated.Mapping {
		segments := segmentsByID[m.ID]
		if segments == nil {
			continue
		}
		replacement := make([]*Mapping, 0, len(segments))
		copiedSegments := make([]*relocationSegment, len(segments))
		for j, segment := range segments {
			newMapping := *m
			newMapping.Start = segment.dstStart
			newMapping.Limit = segment.dstLimit
			newMapping.Offset = segment.offset

			if j > 0 {
				newMapping.ID = nextID + 1
				nextID++
			}

			segment.replacement = &newMapping
			replacement = append(replacement, &newMapping)
			copiedSegments[j] = segment
		}
		replacements[m.ID] = replacement
		segmentsByCopyID[m.ID] = copiedSegments
	}

	newMappings := make([]*Mapping, 0, len(relocated.Mapping))
	for _, m := range relocated.Mapping {
		if replacement := replacements[m.ID]; replacement != nil {
			newMappings = append(newMappings, replacement...)
			continue
		}
		newMappings = append(newMappings, m)
	}
	relocated.Mapping = newMappings

	for _, location := range relocated.Location {
		if location.Mapping == nil {
			continue
		}
		segments := segmentsByCopyID[location.Mapping.ID]
		if segments == nil {
			continue
		}

		segment := findSegment(location.Address, segments)
		if segment == nil {
			return nil, fmt.Errorf("location %d address %#x is outside relocation segments", location.ID, location.Address)
		}
		location.Mapping = segment.replacement
		location.Address = segment.dstStart + (location.Address - segment.srcStart)
	}

	if err := relocated.CheckValid(); err != nil {
		return nil, err
	}
	return relocated, nil
}

func findSegment(address uint64, segments []*relocationSegment) *relocationSegment {
	for _, segment := range segments {
		if address >= segment.srcStart && address < segment.srcLimit {
			return segment
		}
	}
	return nil
}

func addUint64(a, b uint64) (uint64, bool) {
	if b > math.MaxUint64-a {
		return 0, false
	}
	return a + b, true
}
