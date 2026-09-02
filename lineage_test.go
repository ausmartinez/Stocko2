package main

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// countJSONLines checks the log really is one valid JSON object per line.
func countJSONLines(t *testing.T, path string) int {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	n := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var obs Observation
		if err := json.Unmarshal(scanner.Bytes(), &obs); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", n+1, err)
		}
		if obs.Symbol == "" {
			t.Errorf("line %d has no symbol; the embedded candidate did not flatten", n+1)
		}
		n++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestApplyLineagePremarketPhase(t *testing.T) {
	candidates := []GapCandidate{
		{Symbol: "AAA", GapPct: 5, Methods: []string{MethodPercent}},
		{Symbol: "BBB", GapPct: -6, Methods: []string{MethodPercent}},
	}

	applyLineage(candidates, PhasePremarket, nil)

	for _, c := range candidates {
		if !c.SeenPremarket {
			t.Errorf("%s: seen_premarket = false, want true", c.Symbol)
		}
		if c.DiscoveredAt != PhasePremarket {
			t.Errorf("%s: discovered_at = %q, want premarket", c.Symbol, c.DiscoveredAt)
		}
		if c.Faded {
			t.Errorf("%s: faded on the pre-open run", c.Symbol)
		}
	}
}

func TestApplyLineageOpenPhase(t *testing.T) {
	baseline := &ScanResult{
		Phase: PhasePremarket,
		Candidates: []GapCandidate{
			{Symbol: "HELD", GapPct: 6.0, RefPrice: 106, Methods: []string{MethodPercent}},
			{Symbol: "FADE", GapPct: 5.0, RefPrice: 52.5, Methods: []string{MethodPercent}},
		},
	}

	candidates := []GapCandidate{
		// Carried over and still gapping, though the gap grew.
		{Symbol: "HELD", GapPct: 8.0, RefPrice: 108, Methods: []string{MethodPercent, MethodATR}},
		// Carried over but the gap evaporated: no methods fire.
		{Symbol: "FADE", GapPct: 0.4, RefPrice: 50.2},
		// Only visible once the auction printed.
		{Symbol: "NEW", GapPct: -7.0, RefPrice: 93, Methods: []string{MethodPercent}},
	}

	applyLineage(candidates, PhaseOpen, baseline)

	held := candidates[0]
	if !held.SeenPremarket || held.DiscoveredAt != PhasePremarket {
		t.Errorf("HELD: seen=%t discovered=%q, want true/premarket", held.SeenPremarket, held.DiscoveredAt)
	}
	if held.Faded {
		t.Error("HELD: marked faded despite still flagging")
	}
	if math.Abs(held.ProjectedGapPct-6.0) > 1e-9 {
		t.Errorf("HELD: projected_gap_pct = %v, want 6.0", held.ProjectedGapPct)
	}
	if math.Abs(held.GapDeltaPct-2.0) > 1e-9 {
		t.Errorf("HELD: gap_delta_pct = %v, want 2.0", held.GapDeltaPct)
	}

	fade := candidates[1]
	if !fade.SeenPremarket || fade.DiscoveredAt != PhasePremarket {
		t.Errorf("FADE: seen=%t discovered=%q, want true/premarket", fade.SeenPremarket, fade.DiscoveredAt)
	}
	if !fade.Faded {
		t.Error("FADE: not marked faded despite flagging nothing at the open")
	}
	if math.Abs(fade.GapDeltaPct-(-4.6)) > 1e-9 {
		t.Errorf("FADE: gap_delta_pct = %v, want -4.6", fade.GapDeltaPct)
	}

	nw := candidates[2]
	if nw.SeenPremarket {
		t.Error("NEW: marked as seen pre-market")
	}
	if nw.DiscoveredAt != PhaseOpen {
		t.Errorf("NEW: discovered_at = %q, want open", nw.DiscoveredAt)
	}
	if nw.ProjectedGapPct != 0 || nw.GapDeltaPct != 0 {
		t.Error("NEW: projection fields set for a symbol with no pre-open record")
	}
}

// Without a pre-open baseline, lineage must read "unknown" rather than falsely
// claiming everything was newly discovered at the open.
func TestApplyLineageOpenPhaseWithoutBaseline(t *testing.T) {
	candidates := []GapCandidate{{Symbol: "AAA", GapPct: 5, Methods: []string{MethodPercent}}}

	applyLineage(candidates, PhaseOpen, nil)

	if candidates[0].DiscoveredAt != DiscoveredUnknown {
		t.Errorf("discovered_at = %q, want unknown", candidates[0].DiscoveredAt)
	}
	if candidates[0].SeenPremarket {
		t.Error("seen_premarket = true without a baseline to check against")
	}
}

func TestCapCandidatesKeepsCarryForwards(t *testing.T) {
	s := &Scanner{cfg: ScannerConfig{MaxCandidates: 2}}

	got := s.capCandidates([]GapCandidate{
		{Symbol: "A", Methods: []string{MethodPercent}},
		{Symbol: "B", Methods: []string{MethodPercent}},
		{Symbol: "C", Methods: []string{MethodPercent}},
		{Symbol: "FADE1"},
		{Symbol: "FADE2"},
	})

	var flagged, faded int
	for _, c := range got {
		if len(c.Methods) > 0 {
			flagged++
		} else {
			faded++
		}
	}
	if flagged != 2 {
		t.Errorf("flagged = %d, want the cap of 2", flagged)
	}
	if faded != 2 {
		t.Errorf("faded rows = %d, want all 2 kept regardless of the cap", faded)
	}
}

func TestRankAndCapAlwaysKeepsCarried(t *testing.T) {
	s := &Scanner{cfg: ScannerConfig{MaxHistoryLookups: 1}}
	carry := map[string]bool{"CARRIED": true}

	got := s.rankAndCap([]rawGap{
		{symbol: "BIG", gapPct: 20},
		{symbol: "MID", gapPct: 10},
		{symbol: "CARRIED", gapPct: 0.1},
	}, carry)

	found := make(map[string]bool, len(got))
	for _, r := range got {
		found[r.symbol] = true
	}
	if !found["CARRIED"] {
		t.Error("carried symbol dropped by the history cap")
	}
	if !found["BIG"] {
		t.Error("largest gap dropped")
	}
	if len(got) != 2 {
		t.Errorf("got %d rows, want 2 (1 carried + cap of 1)", len(got))
	}
}

func TestPhaseFilePaths(t *testing.T) {
	if got, want := PhaseFile("data", "2026-09-02", PhasePremarket),
		filepath.Join("data", "2026-09-02", "premarket.json"); got != want {
		t.Errorf("PhaseFile = %q, want %q", got, want)
	}
	if got, want := ObservationsFile("data"), filepath.Join("data", "observations.jsonl"); got != want {
		t.Errorf("ObservationsFile = %q, want %q", got, want)
	}
}

func TestLoadScanResultMissingFileIsNotAnError(t *testing.T) {
	got, err := LoadScanResult(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("err = %v, want nil for a missing baseline", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestSaveAndAppendRoundTrip(t *testing.T) {
	dir := t.TempDir()

	result := &ScanResult{
		GeneratedAt: "2026-09-02T09:15:00-04:00",
		SessionDate: "2026-09-02",
		Phase:       PhasePremarket,
		Feed:        "iex",
		Candidates: []GapCandidate{
			{Symbol: "AAA", GapPct: 5, SeenPremarket: true, DiscoveredAt: PhasePremarket},
			{Symbol: "BBB", GapPct: -6, SeenPremarket: true, DiscoveredAt: PhasePremarket},
		},
	}

	path := PhaseFile(dir, result.SessionDate, result.Phase)
	if err := SaveScanResult(path, result); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadScanResult(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || len(loaded.Candidates) != 2 {
		t.Fatalf("loaded = %+v, want 2 candidates", loaded)
	}
	if loaded.Candidates[0].Symbol != "AAA" || loaded.Candidates[0].DiscoveredAt != PhasePremarket {
		t.Errorf("lineage did not survive the round trip: %+v", loaded.Candidates[0])
	}

	// Appending twice must accumulate rather than truncate.
	obs := ObservationsFile(dir)
	if err := AppendObservations(obs, result); err != nil {
		t.Fatal(err)
	}
	if err := AppendObservations(obs, result); err != nil {
		t.Fatal(err)
	}
	if n := countJSONLines(t, obs); n != 4 {
		t.Errorf("observation lines = %d, want 4 after two appends", n)
	}
}
