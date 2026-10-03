package profile

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// partitionTestProfile builds a fresh profile exercising the partition
// edge cases.
//
// pprof stack order reminders used throughout this test:
//   - Sample.Location is ordered leaf-first: Location[0] is the leaf
//     (innermost) frame and the last element is the stack root.
//   - Location.Line lists inlined frames innermost-first: Line[0] is
//     the innermost (leaf-most) frame, Line[len-1] the outermost one,
//     i.e. the frame that made the call at this address.
//
// The fixture uses these logical stacks (rendered root -> leaf):
//
//	s1: main -> entryFoo -> sharedOuter -> sharedMid -> sharedLeaf
//	s2: main -> entryBar -> sharedOuter -> sharedMid -> sharedLeaf
//	s3: main -> worker   -> sharedOuter -> sharedMid -> sharedLeaf
//	s4: main -> entryFoo -> helper -> entryFoo -> sharedOuter -> sharedMid -> sharedLeaf
//	s5: main -> other    -> solo
//	s6: main -> entryMisc -> solo
//
// s1 and s2 share locShared, a single Location with a three-deep
// inline stack (sharedOuter called sharedMid which called sharedLeaf),
// but enter it from different roots. s3 has no entry frame above
// locShared, so its entry (sharedMid) is found in the middle of the
// inline stack. s4 re-enters entryFoo recursively. s5 matches no rule.
func partitionTestProfile() *Profile {
	main := &Function{ID: 1, Name: "main", SystemName: "main", Filename: "main.go"}
	entryFoo := &Function{ID: 2, Name: "entryFoo", SystemName: "entryFoo", Filename: "foo.go"}
	entryBar := &Function{ID: 3, Name: "entryBar", SystemName: "entryBar", Filename: "bar.go"}
	worker := &Function{ID: 4, Name: "worker", SystemName: "worker", Filename: "worker.go"}
	helper := &Function{ID: 5, Name: "helper", SystemName: "helper", Filename: "helper.go"}
	sharedOuter := &Function{ID: 6, Name: "sharedOuter", SystemName: "sharedOuter", Filename: "shared.go"}
	sharedMid := &Function{ID: 7, Name: "sharedMid", SystemName: "sharedMid", Filename: "shared.go"}
	sharedLeaf := &Function{ID: 8, Name: "sharedLeaf", SystemName: "sharedLeaf", Filename: "shared.go"}
	entryMisc := &Function{ID: 9, Name: "entryMisc", SystemName: "entryMisc", Filename: "misc.go"}
	other := &Function{ID: 10, Name: "other", SystemName: "other", Filename: "other.go"}
	solo := &Function{ID: 11, Name: "solo", SystemName: "solo", Filename: "solo.go"}

	locMain := &Location{ID: 1, Line: []Line{{Function: main, Line: 10}}}
	locFoo := &Location{ID: 2, Line: []Line{{Function: entryFoo, Line: 20}}}
	locBar := &Location{ID: 3, Line: []Line{{Function: entryBar, Line: 30}}}
	locWorker := &Location{ID: 4, Line: []Line{{Function: worker, Line: 40}}}
	locHelper := &Location{ID: 5, Line: []Line{{Function: helper, Line: 50}}}
	// locShared carries the whole inline stack of one call site:
	// Line[0] is the innermost frame (sharedLeaf), Line[2] the
	// outermost one (sharedOuter).
	locShared := &Location{ID: 6, Line: []Line{
		{Function: sharedLeaf, Line: 61},
		{Function: sharedMid, Line: 62},
		{Function: sharedOuter, Line: 63},
	}}
	locMisc := &Location{ID: 7, Line: []Line{{Function: entryMisc, Line: 70}}}
	locOther := &Location{ID: 8, Line: []Line{{Function: other, Line: 80}}}
	locSolo := &Location{ID: 9, Line: []Line{{Function: solo, Line: 90}}}

	return &Profile{
		PeriodType:    &ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:        1,
		DurationNanos: 10e9,
		SampleType: []*ValueType{
			{Type: "samples", Unit: "count"},
			{Type: "cpu", Unit: "nanoseconds"},
		},
		Sample: []*Sample{
			// s1: enters via entryFoo; the cut lands on a location
			// boundary, locShared is kept whole.
			{
				Location: []*Location{locShared, locFoo, locMain},
				Value:    []int64{1, 10},
				Label:    map[string][]string{"route": {"foo"}},
				NumLabel: map[string][]int64{"bytes": {7}},
				NumUnit:  map[string][]string{"bytes": {"bytes"}},
			},
			// s2: shares locShared with s1 but enters via entryBar.
			{
				Location: []*Location{locShared, locBar, locMain},
				Value:    []int64{2, 20},
				Label:    map[string][]string{"route": {"bar"}},
			},
			// s3: no entry above locShared; the entry sharedMid is
			// found in the middle of locShared's inline stack, so the
			// cut lands between Line[1] and Line[2] of the location.
			{
				Location: []*Location{locShared, locWorker, locMain},
				Value:    []int64{4, 40},
			},
			// s4: entryFoo recurses through helper; the root-most
			// entryFoo wins and the sample is counted only once.
			{
				Location: []*Location{locShared, locFoo, locHelper, locFoo, locMain},
				Value:    []int64{8, 80},
			},
			// s5: matches no rule, keeps its full stack.
			{
				Location: []*Location{locSolo, locOther, locMain},
				Value:    []int64{16, 160},
			},
			// s6: entryMisc only matches the overlapping catch-all
			// rule "entry".
			{
				Location: []*Location{locSolo, locMisc, locMain},
				Value:    []int64{32, 320},
			},
		},
		Location: []*Location{locMain, locFoo, locBar, locWorker, locHelper, locShared, locMisc, locOther, locSolo},
		Function: []*Function{main, entryFoo, entryBar, worker, helper, sharedOuter, sharedMid, sharedLeaf, entryMisc, other, solo},
	}
}

// partitionTestRules returns the ordered rules used by the partition
// tests. "entry" overlaps "foo" and "bar": entryFoo/entryBar frames
// match it too, but the earlier rules must win.
func partitionTestRules() []PartitionRule {
	return []PartitionRule{
		{Name: "foo", Re: regexp.MustCompile(`^entryFoo$`)},
		{Name: "bar", Re: regexp.MustCompile(`^entryBar$`)},
		{Name: "mid", Re: regexp.MustCompile(`^sharedMid$`)},
		{Name: "entry", Re: regexp.MustCompile(`^entry`)},
	}
}

// stackString renders a sample stack from the root to the leaf,
// expanding inlined frames, with frames joined by " > ".
func stackString(s *Sample) string {
	var frames []string
	for i := len(s.Location) - 1; i >= 0; i-- {
		loc := s.Location[i]
		for j := len(loc.Line) - 1; j >= 0; j-- {
			name := "<nil>"
			if fn := loc.Line[j].Function; fn != nil {
				name = fn.Name
			}
			frames = append(frames, name)
		}
	}
	return strings.Join(frames, " > ")
}

func sampleStacks(p *Profile) []string {
	stacks := make([]string, 0, len(p.Sample))
	for _, s := range p.Sample {
		stacks = append(stacks, stackString(s))
	}
	return stacks
}

func sumValues(p *Profile) []int64 {
	sum := make([]int64, len(p.SampleType))
	for _, s := range p.Sample {
		for i, v := range s.Value {
			sum[i] += v
		}
	}
	return sum
}

func TestPartition(t *testing.T) {
	in := partitionTestProfile()
	wantSum := sumValues(in)

	parts, unmatched := in.Partition(partitionTestRules())

	// Every output, including the unmatched profile, must be valid.
	for name, part := range parts {
		if err := part.CheckValid(); err != nil {
			t.Errorf("partition %q: %v", name, err)
		}
	}
	if err := unmatched.CheckValid(); err != nil {
		t.Errorf("unmatched profile: %v", err)
	}

	// The stacks kept by each partition, rendered root -> leaf.
	wantStacks := map[string][]string{
		"foo": {
			"entryFoo > sharedOuter > sharedMid > sharedLeaf",
			"entryFoo > helper > entryFoo > sharedOuter > sharedMid > sharedLeaf",
		},
		"bar": {"entryBar > sharedOuter > sharedMid > sharedLeaf"},
		// The cut landed in the middle of locShared's inline stack:
		// sharedOuter is gone, sharedMid is the new root.
		"mid":   {"sharedMid > sharedLeaf"},
		"entry": {"entryMisc > solo"},
	}
	for name, want := range wantStacks {
		part, ok := parts[name]
		if !ok {
			t.Errorf("missing partition %q", name)
			continue
		}
		if got := sampleStacks(part); !reflect.DeepEqual(got, want) {
			t.Errorf("partition %q stacks = %q, want %q", name, got, want)
		}
	}
	// The unmatched profile keeps the full stack, root included.
	if got, want := sampleStacks(unmatched), []string{"main > other > solo"}; !reflect.DeepEqual(got, want) {
		t.Errorf("unmatched stacks = %q, want %q", got, want)
	}

	// Each input sample lands in exactly one output, and the per-column
	// sums over all outputs strictly equal the input sums.
	var n int
	gotSum := sumValues(unmatched)
	n += len(unmatched.Sample)
	for _, part := range parts {
		n += len(part.Sample)
		for i, v := range sumValues(part) {
			gotSum[i] += v
		}
	}
	if n != len(in.Sample) {
		t.Errorf("outputs hold %d samples, want %d", n, len(in.Sample))
	}
	if !reflect.DeepEqual(gotSum, wantSum) {
		t.Errorf("output value sums = %v, want %v", gotSum, wantSum)
	}

	// Values and labels follow their sample unchanged.
	s1 := parts["foo"].Sample[0]
	if got, want := s1.Value, []int64{1, 10}; !reflect.DeepEqual(got, want) {
		t.Errorf("foo sample values = %v, want %v", got, want)
	}
	if got := s1.Label["route"]; !reflect.DeepEqual(got, []string{"foo"}) {
		t.Errorf("foo sample label route = %v, want [foo]", got)
	}
	if got := s1.NumLabel["bytes"]; !reflect.DeepEqual(got, []int64{7}) {
		t.Errorf("foo sample numlabel bytes = %v, want [7]", got)
	}
	if got := s1.NumUnit["bytes"]; !reflect.DeepEqual(got, []string{"bytes"}) {
		t.Errorf("foo sample numunit bytes = %v, want [bytes]", got)
	}

	// The input profile is untouched: locShared still has its three
	// inlined lines and every sample its full stack.
	if got := len(in.Location[5].Line); got != 3 {
		t.Errorf("input locShared has %d lines, want 3", got)
	}
	for i, want := range []int{3, 3, 3, 5, 3, 3} {
		if got := len(in.Sample[i].Location); got != want {
			t.Errorf("input sample %d has %d locations, want %d", i, got, want)
		}
	}

	// Outputs are independent of the input and of each other: mutate
	// the "foo" partition and check nothing else changes.
	foo := parts["foo"]
	foo.Sample[0].Value[0] = 999
	foo.Sample[0].Label["route"][0] = "changed"
	for _, f := range foo.Function {
		if f.Name == "sharedMid" {
			f.Name = "hacked"
		}
	}
	if got := in.Sample[0].Value[0]; got != 1 {
		t.Errorf("input sample value changed to %d", got)
	}
	if got := in.Sample[0].Label["route"][0]; got != "foo" {
		t.Errorf("input sample label changed to %q", got)
	}
	if got := in.Function[6].Name; got != "sharedMid" {
		t.Errorf("input function renamed to %q", got)
	}
	for _, f := range parts["mid"].Function {
		if f.Name == "hacked" {
			t.Errorf("mutation of partition foo leaked into partition mid")
		}
	}
}

// TestPartitionTrimmedLocationShared checks that two samples cut at
// the same inlined frame of a shared location produce a single trimmed
// location in the output.
func TestPartitionTrimmedLocationShared(t *testing.T) {
	in := partitionTestProfile()
	// Duplicate s3: both samples must be cut at sharedMid inside the
	// shared location's inline stack.
	dup := *in.Sample[2]
	in.Sample = append(in.Sample, &dup)

	parts, _ := in.Partition(partitionTestRules())
	mid := parts["mid"]
	if err := mid.CheckValid(); err != nil {
		t.Fatal(err)
	}
	if got := len(mid.Location); got != 1 {
		t.Fatalf("mid partition has %d locations, want 1", got)
	}
	if got := len(mid.Location[0].Line); got != 2 {
		t.Errorf("trimmed location has %d lines, want 2", got)
	}
	// The input location keeps its full inline stack.
	if got := len(in.Location[5].Line); got != 3 {
		t.Errorf("input locShared has %d lines, want 3", got)
	}
}
