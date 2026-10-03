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
	"reflect"
	"regexp"
	"testing"
)

// stackNames returns the logical stack of a sample from root to leaf,
// expanding inlined frames. In pprof a sample's locations are stored
// leaf-first (Location[0] is the leaf, Location[len-1] is the root),
// and within a location Line[0] is the innermost (leaf) function while
// subsequent lines are its inlined callers, so the root-to-leaf order
// walks both slices backwards.
func stackNames(s *Sample) []string {
	var names []string
	for li := len(s.Location) - 1; li >= 0; li-- {
		loc := s.Location[li]
		for ln := len(loc.Line) - 1; ln >= 0; ln-- {
			names = append(names, loc.Line[ln].Function.Name)
		}
	}
	return names
}

// sampleSums returns the per-value-column sums of all samples.
func sampleSums(p *Profile) []int64 {
	sums := make([]int64, len(p.SampleType))
	for _, s := range p.Sample {
		for i, v := range s.Value {
			sums[i] += v
		}
	}
	return sums
}

// partitionTestProfile builds a profile exercising the partition
// rules:
//
//   - S1 and S2 share the location sharedLoc, which carries three
//     logical frames thanks to two levels of inlining, but enter it
//     from different root paths.
//   - S3 re-enters the "api" entry recursively.
//   - The "api" and "worker" rules overlap: app/api.* frames match
//     both, and the rule order must decide.
//   - S5 matches no rule at all.
//
// Logical stacks (root -> leaf), remembering that pprof stores
// locations leaf-first and inline lines innermost-first:
//
//	S1: main.main -> app.dispatch -> app/api.handle -> app.serve
//	S2: app/api.boot -> app.dispatch -> app/api.handle -> app.serve
//	S3: main.main -> app.dispatch -> app/api.handle -> app.serve -> app/api.recur
//	S4: main.main -> app/worker.run -> app.work
//	S5: main.main -> app/db.query
func partitionTestProfile() *Profile {
	fns := map[string]*Function{}
	fn := func(name, file string) *Function {
		f := &Function{
			ID:         uint64(len(fns)) + 1,
			Name:       name,
			SystemName: name,
			Filename:   file,
		}
		fns[name] = f
		return f
	}
	mainMain := fn("main.main", "main.go")
	apiBoot := fn("app/api.boot", "api.go")
	dispatch := fn("app.dispatch", "dispatch.go")
	apiHandle := fn("app/api.handle", "api.go")
	serve := fn("app.serve", "serve.go")
	apiRecur := fn("app/api.recur", "api.go")
	workerRun := fn("app/worker.run", "worker.go")
	work := fn("app.work", "work.go")
	dbQuery := fn("app/db.query", "db.go")

	m := &Mapping{
		ID:              1,
		Start:           0x1000,
		Limit:           0x9000,
		File:            "/bin/app",
		HasFunctions:    true,
		HasFilenames:    true,
		HasLineNumbers:  true,
		HasInlineFrames: true,
	}

	// sharedLoc is referenced by both S1 and S2. Its lines are stored
	// innermost-first: serve is the real leaf function, api.handle is
	// inlined into dispatch, and dispatch is the outermost frame.
	sharedLoc := &Location{
		ID:      1,
		Mapping: m,
		Address: 0x2000,
		Line: []Line{
			{Function: serve, Line: 10},
			{Function: apiHandle, Line: 20},
			{Function: dispatch, Line: 30},
		},
	}
	rootMain := &Location{
		ID:      2,
		Mapping: m,
		Address: 0x3000,
		Line:    []Line{{Function: mainMain, Line: 5}},
	}
	rootAPI := &Location{
		ID:      3,
		Mapping: m,
		Address: 0x4000,
		Line:    []Line{{Function: apiBoot, Line: 6}},
	}
	recurLoc := &Location{
		ID:      4,
		Mapping: m,
		Address: 0x5000,
		Line:    []Line{{Function: apiRecur, Line: 7}},
	}
	workLoc := &Location{
		ID:      5,
		Mapping: m,
		Address: 0x6000,
		Line: []Line{
			{Function: work, Line: 11},
			{Function: workerRun, Line: 12},
		},
	}
	dbLoc := &Location{
		ID:      6,
		Mapping: m,
		Address: 0x7000,
		Line:    []Line{{Function: dbQuery, Line: 8}},
	}

	p := &Profile{
		SampleType: []*ValueType{
			{Type: "samples", Unit: "count"},
			{Type: "cpu", Unit: "nanoseconds"},
		},
		PeriodType: &ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:     1,
		Mapping:    []*Mapping{m},
		Location:   []*Location{sharedLoc, rootMain, rootAPI, recurLoc, workLoc, dbLoc},
	}
	for _, f := range fns {
		p.Function = append(p.Function, f)
	}

	p.Sample = []*Sample{
		// S1: hits "api" mid-inline inside sharedLoc (app/api.handle).
		{
			Location: []*Location{sharedLoc, rootMain},
			Value:    []int64{1, 100},
			Label:    map[string][]string{"entry": {"s1"}},
			NumLabel: map[string][]int64{"retries": {2}},
			NumUnit:  map[string][]string{"retries": {"count"}},
		},
		// S2: hits "api" at the root location, above sharedLoc.
		{
			Location: []*Location{sharedLoc, rootAPI},
			Value:    []int64{2, 200},
			Label:    map[string][]string{"entry": {"s2"}},
		},
		// S3: hits "api" inside sharedLoc and recursively re-enters it
		// at the leaf; must be attributed (and counted) only once.
		{
			Location: []*Location{recurLoc, sharedLoc, rootMain},
			Value:    []int64{4, 400},
		},
		// S4: hits "worker" at the outermost inline line of workLoc.
		{
			Location: []*Location{workLoc, rootMain},
			Value:    []int64{8, 800},
		},
		// S5: matches no entry.
		{
			Location: []*Location{dbLoc, rootMain},
			Value:    []int64{16, 1600},
		},
	}
	return p
}

func partitionTestEntries() []PartitionEntry {
	return []PartitionEntry{
		// The rules overlap: app/api.* frames match both entries, and
		// the first entry must win.
		{Name: "api", Match: regexp.MustCompile(`^app/api\.`)},
		{Name: "worker", Match: regexp.MustCompile(`^app/(worker|api)\.`)},
	}
}

func TestPartition(t *testing.T) {
	p := partitionTestProfile()
	entries := partitionTestEntries()

	if err := p.CheckValid(); err != nil {
		t.Fatalf("input profile is invalid: %v", err)
	}
	inputSums := sampleSums(p)

	parts, unmatched := p.Partition(entries)
	if len(parts) != len(entries) {
		t.Fatalf("got %d partitions, want %d", len(parts), len(entries))
	}
	api, worker := parts[0], parts[1]

	// Every output must be a valid profile, and the per-column sums of
	// all outputs together must equal the input sums exactly.
	total := make([]int64, len(inputSums))
	for name, out := range map[string]*Profile{"api": api, "worker": worker, "unmatched": unmatched} {
		if err := out.CheckValid(); err != nil {
			t.Errorf("partition %q is invalid: %v", name, err)
		}
		for i, v := range sampleSums(out) {
			total[i] += v
		}
	}
	if !reflect.DeepEqual(total, inputSums) {
		t.Errorf("output column sums = %v, want input sums %v", total, inputSums)
	}

	// Expected attribution: S1+S2+S3 -> api, S4 -> worker, S5 -> unmatched.
	if got, want := sampleSums(api), []int64{7, 700}; !reflect.DeepEqual(got, want) {
		t.Errorf("api sums = %v, want %v", got, want)
	}
	if got, want := sampleSums(worker), []int64{8, 800}; !reflect.DeepEqual(got, want) {
		t.Errorf("worker sums = %v, want %v", got, want)
	}
	if got, want := sampleSums(unmatched), []int64{16, 1600}; !reflect.DeepEqual(got, want) {
		t.Errorf("unmatched sums = %v, want %v", got, want)
	}

	// Collect the api stacks (root -> leaf) for comparison.
	var apiStacks [][]string
	for _, s := range api.Sample {
		apiStacks = append(apiStacks, stackNames(s))
	}
	wantAPIStacks := [][]string{
		// S1: cut mid-inline inside the shared location; the frames
		// above the hit (main.main, app.dispatch) are gone.
		{"app/api.handle", "app.serve"},
		// S2: hit at the root, so the full stack is kept, including
		// all three inline lines of the shared location.
		{"app/api.boot", "app.dispatch", "app/api.handle", "app.serve"},
		// S3: the recursive re-entry (app/api.recur) stays in the
		// stack but does not attribute the sample a second time.
		{"app/api.handle", "app.serve", "app/api.recur"},
	}
	for _, want := range wantAPIStacks {
		found := false
		for _, got := range apiStacks {
			if reflect.DeepEqual(got, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("api partition missing stack %v, got stacks %v", want, apiStacks)
		}
	}

	// The worker sample is cut at the outermost inline line of
	// workLoc, keeping the whole location.
	if len(worker.Sample) != 1 {
		t.Fatalf("worker partition has %d samples, want 1", len(worker.Sample))
	}
	if got, want := stackNames(worker.Sample[0]), []string{"app/worker.run", "app.work"}; !reflect.DeepEqual(got, want) {
		t.Errorf("worker stack = %v, want %v", got, want)
	}

	// The unmatched sample keeps its complete stack.
	if len(unmatched.Sample) != 1 {
		t.Fatalf("unmatched partition has %d samples, want 1", len(unmatched.Sample))
	}
	if got, want := stackNames(unmatched.Sample[0]), []string{"main.main", "app/db.query"}; !reflect.DeepEqual(got, want) {
		t.Errorf("unmatched stack = %v, want %v", got, want)
	}

	// Labels and values are carried over unchanged.
	var s1 *Sample
	for _, s := range api.Sample {
		if s.Label["entry"] != nil && s.Label["entry"][0] == "s1" {
			s1 = s
		}
	}
	if s1 == nil {
		t.Fatalf("api partition is missing sample S1")
	}
	if got, want := s1.Value, []int64{1, 100}; !reflect.DeepEqual(got, want) {
		t.Errorf("S1 values = %v, want %v", got, want)
	}
	if got, want := s1.NumLabel["retries"], []int64{2}; !reflect.DeepEqual(got, want) {
		t.Errorf("S1 num label = %v, want %v", got, want)
	}
	if got, want := s1.NumUnit["retries"], []string{"count"}; !reflect.DeepEqual(got, want) {
		t.Errorf("S1 num unit = %v, want %v", got, want)
	}

	// The input profile must not be mutated: the shared location
	// still has all three inline lines and the samples their original
	// stacks.
	if got := len(p.Location[0].Line); got != 3 {
		t.Errorf("input shared location has %d lines after Partition, want 3", got)
	}
	if got, want := stackNames(p.Sample[0]), []string{"main.main", "app.dispatch", "app/api.handle", "app.serve"}; !reflect.DeepEqual(got, want) {
		t.Errorf("input S1 stack = %v after Partition, want %v", got, want)
	}

	// Outputs are independent: mutating one must not affect the input
	// or the other outputs. Rename a function in the api partition.
	renamed := false
	for _, f := range api.Function {
		if f.Name == "app/api.handle" {
			f.Name = "app/api.renamed"
			renamed = true
		}
	}
	if !renamed {
		t.Fatalf("api partition has no app/api.handle function")
	}
	for name, fn := range map[string]string{"input": "app/api.handle", "worker": "app/worker.run"} {
		var src *Profile
		switch name {
		case "input":
			src = p
		case "worker":
			src = worker
		}
		found := false
		for _, f := range src.Function {
			if f.Name == fn {
				found = true
			}
		}
		if !found {
			t.Errorf("%s profile lost function %q after mutating the api partition", name, fn)
		}
	}
}

// TestPartitionEmptyEntries checks that with no entries every sample
// lands in the unmatched profile with its stack intact.
func TestPartitionEmptyEntries(t *testing.T) {
	p := partitionTestProfile()
	parts, unmatched := p.Partition(nil)
	if len(parts) != 0 {
		t.Fatalf("got %d partitions, want 0", len(parts))
	}
	if err := unmatched.CheckValid(); err != nil {
		t.Fatalf("unmatched profile is invalid: %v", err)
	}
	if got, want := sampleSums(unmatched), sampleSums(p); !reflect.DeepEqual(got, want) {
		t.Errorf("unmatched sums = %v, want %v", got, want)
	}
	if got, want := stackNames(unmatched.Sample[0]), stackNames(p.Sample[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("unmatched stack = %v, want %v", got, want)
	}
}
