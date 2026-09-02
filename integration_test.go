package main

import (
	"math"
	"os"
	"testing"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/joho/godotenv"
)

// Integration tests hit the live Alpaca API and are skipped unless
// ALPACA_INTEGRATION=1 is set:
//
//	ALPACA_INTEGRATION=1 go test -run Integration -v ./...
func requireIntegration(t *testing.T) *alpaca.Client {
	t.Helper()

	if os.Getenv("ALPACA_INTEGRATION") != "1" {
		t.Skip("set ALPACA_INTEGRATION=1 to run tests against the live API")
	}
	_ = godotenv.Load()

	if os.Getenv("APCA_API_KEY_ID") == "" || os.Getenv("APCA_API_SECRET_KEY") == "" {
		t.Skip("APCA_API_KEY_ID / APCA_API_SECRET_KEY not set")
	}

	return alpaca.NewClient(alpaca.ClientOpts{
		BaseURL: "https://paper-api.alpaca.markets",
	})
}

func TestIntegrationResolveSession(t *testing.T) {
	client := requireIntegration(t)

	session, err := ResolveSession(client, "04:00")
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if session.Date == "" || session.Open.IsZero() || session.Close.IsZero() {
		t.Fatalf("incomplete session: %+v", session)
	}
	if !session.Open.Before(session.Close) {
		t.Errorf("open %v is not before close %v", session.Open, session.Close)
	}
	// Whichever session we resolved to, it must not already be over.
	if !session.Now.Before(session.Close) {
		t.Errorf("resolved a session that already closed: now=%v close=%v", session.Now, session.Close)
	}

	t.Logf("session=%s open=%s close=%s mode=%s",
		session.Date, session.Open.Format(time.RFC3339), session.Close.Format(time.RFC3339), session.Mode())
}

func TestIntegrationUniverse(t *testing.T) {
	client := requireIntegration(t)

	cfg := DefaultScannerConfig()
	scanner, err := NewScanner(client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Close()

	symbols, err := scanner.Universe()
	if err != nil {
		t.Fatalf("Universe: %v", err)
	}
	if len(symbols) < 1000 {
		t.Errorf("universe of %d symbols looks too small", len(symbols))
	}

	t.Logf("universe = %d symbols, first few: %v", len(symbols), symbols[:5])
}

// loosenedScanner returns a scanner whose thresholds are low enough that the
// shape of the output is what is under test, not whether a particular name
// happened to gap today.
func loosenedScanner(t *testing.T, client *alpaca.Client, dataDir string) (*Scanner, ScannerConfig) {
	t.Helper()

	cfg := DefaultScannerConfig()
	cfg.DataDir = dataDir
	cfg.SymbolsOverride = []string{
		"AAPL", "TSLA", "NVDA", "AMD", "INTC", "F", "PLTR", "SOFI", "BAC", "T",
		"MARA", "RIOT", "COIN", "NIO", "RIVN", "LCID",
	}
	cfg.PrescreenGapPct = 0.01
	cfg.MinPrevVolume = 0
	cfg.Thresholds = GapThresholds{GapPct: 0.05, ATRMult: 0.01, Sigma: 0.01}
	cfg.MinSigmaSamples = 5

	scanner, err := NewScanner(client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return scanner, cfg
}

// TestIntegrationScanCurrentSession runs the whole pipeline against whichever
// session is live right now, so both the projected and confirmed paths get
// exercised depending on when it is run.
func TestIntegrationScanCurrentSession(t *testing.T) {
	client := requireIntegration(t)

	scanner, cfg := loosenedScanner(t, client, t.TempDir())
	defer scanner.Close()

	session, err := ResolveSession(client, cfg.PremarketStartET)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scanning session %s, phase %s", session.Date, session.Mode())

	result, err := scanner.Scan(session)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if result.SnapshotsWithData == 0 {
		t.Fatal("no snapshots returned usable daily bars")
	}
	if len(result.Candidates) == 0 {
		if result.Phase == PhasePremarket {
			t.Skipf("nothing has printed yet for session %s (%d symbols not yet traded); "+
				"re-run closer to the open", session.Date, result.NotYetTraded)
		}
		t.Fatal("no candidates at near-zero thresholds; the pipeline is not producing data")
	}

	wantSources := map[string]bool{"premarket_last_trade": true, "premarket_daily_bar": true}
	if result.Phase == PhaseOpen {
		wantSources = map[string]bool{"session_open_daily_bar": true, "opening_auction": true, "latest_trade": true}
	}

	var withATR, withSigma, withPremarket int
	for _, c := range result.Candidates {
		if c.PrevClose <= 0 || c.RefPrice <= 0 {
			t.Errorf("%s: bad prices prev=%v ref=%v", c.Symbol, c.PrevClose, c.RefPrice)
		}
		if !wantSources[c.RefSource] {
			t.Errorf("%s: ref_source = %q, unexpected for phase %s", c.Symbol, c.RefSource, result.Phase)
		}
		if c.PrevDate >= session.Date {
			t.Errorf("%s: prev session %q is not before the scanned session %q", c.Symbol, c.PrevDate, session.Date)
		}
		if c.ATR > 0 {
			withATR++
		}
		if c.SigmaSamples > 0 {
			withSigma++
		}
		if c.PremarketBars > 0 {
			withPremarket++
		}

		t.Logf("%-6s %-4s %-22s prev=%8.2f ref=%8.2f gap=%6.2f%% xatr=%5.2f sigma=%6.2f relvol=%5.2f methods=%v",
			c.Symbol, c.Direction, c.RefSource, c.PrevClose, c.RefPrice, c.GapPct,
			c.GapATR, c.GapSigma, c.PrevVolumeRatio, c.Methods)
	}

	if withATR == 0 {
		t.Error("no candidate got an ATR; the daily history lookup is not working")
	}
	if withSigma == 0 {
		t.Error("no candidate got overnight stats; the sigma method is not working")
	}
	if withPremarket == 0 {
		t.Error("no candidate got pre-market bars; the minute bar lookup is not working")
	}

	t.Logf("rows=%d withATR=%d withSigma=%d withPremarket=%d",
		len(result.Candidates), withATR, withSigma, withPremarket)
}

// TestIntegrationTwoPhaseLineage plants a pre-open baseline, runs the real open
// scan against it, and checks that carried-over and newly-found gappers are
// labelled correctly on live data.
func TestIntegrationTwoPhaseLineage(t *testing.T) {
	client := requireIntegration(t)

	scanner, cfg := loosenedScanner(t, client, t.TempDir())
	defer scanner.Close()

	session, err := ResolveSession(client, cfg.PremarketStartET)
	if err != nil {
		t.Fatal(err)
	}
	if session.BeforeOpen {
		t.Skipf("session %s has not opened yet; this test covers the open phase", session.Date)
	}

	// The baseline the open run will read back. AAPL and NIO are "carried",
	// the rest of the override list should come out as new at the open.
	carried := map[string]bool{"AAPL": true, "NIO": true}
	baseline := &ScanResult{
		GeneratedAt: session.Now.Format(time.RFC3339),
		SessionDate: session.Date,
		Phase:       PhasePremarket,
		Feed:        cfg.Feed,
		Candidates: []GapCandidate{
			{Symbol: "AAPL", GapPct: 4.5, RefPrice: 331.0, SeenPremarket: true, DiscoveredAt: PhasePremarket},
			{Symbol: "NIO", GapPct: -3.0, RefPrice: 4.12, SeenPremarket: true, DiscoveredAt: PhasePremarket},
		},
	}
	if err := SaveScanResult(PhaseFile(cfg.DataDir, session.Date, PhasePremarket), baseline); err != nil {
		t.Fatal(err)
	}

	result, err := scanner.Scan(session)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if result.Phase != PhaseOpen {
		t.Fatalf("phase = %q, want open", result.Phase)
	}
	if !result.PremarketScanFound {
		t.Fatal("premarket_scan_found = false; the open run did not read the baseline")
	}
	if result.PremarketCandidates != len(carried) {
		t.Errorf("premarket_candidates = %d, want %d", result.PremarketCandidates, len(carried))
	}

	bySymbol := make(map[string]GapCandidate, len(result.Candidates))
	for _, c := range result.Candidates {
		bySymbol[c.Symbol] = c
	}

	for sym := range carried {
		c, ok := bySymbol[sym]
		if !ok {
			t.Errorf("%s: carried-forward symbol missing from the open run entirely", sym)
			continue
		}
		if !c.SeenPremarket || c.DiscoveredAt != PhasePremarket {
			t.Errorf("%s: seen=%t discovered=%q, want true/premarket", sym, c.SeenPremarket, c.DiscoveredAt)
		}
		if c.ProjectedGapPct == 0 {
			t.Errorf("%s: projected_gap_pct not carried over", sym)
		}
		if got, want := c.GapDeltaPct, c.GapPct-c.ProjectedGapPct; math.Abs(got-want) > 1e-9 {
			t.Errorf("%s: gap_delta_pct = %v, want %v", sym, got, want)
		}
		t.Logf("%-5s carried: projected=%6.2f%% actual=%6.2f%% delta=%6.2f%% faded=%t methods=%v",
			sym, c.ProjectedGapPct, c.GapPct, c.GapDeltaPct, c.Faded, c.Methods)
	}

	if result.NewAtOpen == 0 {
		t.Error("new_at_open = 0; nothing was labelled as discovered at the open")
	}
	for _, c := range result.Candidates {
		if carried[c.Symbol] {
			continue
		}
		if c.DiscoveredAt != PhaseOpen {
			t.Errorf("%s: discovered_at = %q, want open", c.Symbol, c.DiscoveredAt)
		}
		if c.SeenPremarket {
			t.Errorf("%s: seen_premarket = true but it was not on the baseline", c.Symbol)
		}
	}

	// Context features should be populated from the history pull.
	for _, c := range result.Candidates {
		if c.AvgVolume == 0 {
			t.Errorf("%s: avg_volume not computed", c.Symbol)
		}
		if c.PrevVolumeRatio == 0 {
			t.Errorf("%s: prev_volume_ratio not computed", c.Symbol)
		}
	}

	t.Logf("rows=%d new_at_open=%d faded=%d", len(result.Candidates), result.NewAtOpen, result.FadedCount)
}
