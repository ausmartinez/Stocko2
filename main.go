package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/joho/godotenv"
)

// defaultConfig is the config written on first run.
func defaultConfig() Config {
	return Config{
		AppName:   "MyGoApp2",
		Version:   "1.0.0",
		Debug:     false,
		Positions: []Position{},
		Scanner:   DefaultScannerConfig(),
	}
}

// loadConfig reads the config file, creating it with defaults if it is missing.
func loadConfig(filename string) (Config, error) {
	data, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		config := defaultConfig()
		log.Printf("'%s' not found, creating one with defaults", filename)

		defaultData, marshalErr := json.MarshalIndent(config, "", "    ")
		if marshalErr != nil {
			return Config{}, fmt.Errorf("encoding default config: %w", marshalErr)
		}
		if writeErr := os.WriteFile(filename, defaultData, 0o644); writeErr != nil {
			return Config{}, fmt.Errorf("writing %s: %w", filename, writeErr)
		}
		return config, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("reading %s: %w", filename, err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", filename, err)
	}
	// Fill in anything the file predates or leaves out.
	applyScannerDefaults(&config.Scanner)

	log.Printf("'%s' found, configuration loaded", filename)
	return config, nil
}

func setupLogging() *os.File {
	logFile, err := os.OpenFile("app.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Fatal("Failed to open log file: ", err)
	}
	log.SetOutput(logFile)
	return logFile
}

// runScan resolves the session, scans for gappers and writes the watchlist.
func runScan(client *alpaca.Client, cfg ScannerConfig) error {
	session, err := ResolveSession(client, cfg.PremarketStartET)
	if err != nil {
		return err
	}

	scanner, err := NewScanner(client, cfg)
	if err != nil {
		return err
	}
	defer scanner.Close()

	result, err := scanner.Scan(session)
	if err != nil {
		return err
	}

	// Per-session phase file: the pre-open run writes the baseline the open run
	// reads back to work out lineage.
	phasePath := PhaseFile(cfg.DataDir, result.SessionDate, result.Phase)
	if err := SaveScanResult(phasePath, result); err != nil {
		return err
	}
	obsPath := ObservationsFile(cfg.DataDir)
	if err := AppendObservations(obsPath, result); err != nil {
		return err
	}
	if err := WriteWatchlist(cfg.WatchlistFile, result); err != nil {
		return err
	}

	log.Printf("scanner: wrote %d rows to %s, %s and %s",
		len(result.Candidates), phasePath, obsPath, cfg.WatchlistFile)
	PrintWatchlist(result)
	return nil
}

// runOutcomes scores a completed session against the scan that flagged it.
func runOutcomes(client *alpaca.Client, cfg ScannerConfig, date string) error {
	var (
		session TradingSession
		err     error
	)
	if date != "" {
		session, err = ResolveSessionByDate(client, date, cfg.PremarketStartET)
	} else {
		session, err = ResolveLastCompletedSession(client, cfg.PremarketStartET)
	}
	if err != nil {
		return err
	}

	// Score against the open run if there is one, since it holds the confirmed
	// gaps plus the faded pre-open candidates.
	scan, err := loadScanForOutcomes(cfg.DataDir, session.Date)
	if err != nil {
		return err
	}

	scanner, err := NewScanner(client, cfg)
	if err != nil {
		return err
	}
	defer scanner.Close()

	outcomes, tracks, err := scanner.CollectOutcomes(session, scan)
	if err != nil {
		return err
	}
	if len(outcomes) == 0 {
		log.Printf("outcomes: no intraday data for %s", session.Date)
		return nil
	}

	portfolio := BuildPortfolio(session, scan, outcomes, cfg)

	if err := SaveJSON(PhaseFile(cfg.DataDir, session.Date, PhaseOutcome), outcomes); err != nil {
		return err
	}
	if err := SaveJSON(SessionFile(cfg.DataDir, session.Date, "tracks"), tracks); err != nil {
		return err
	}
	if err := SaveJSON(SessionFile(cfg.DataDir, session.Date, "portfolio"), portfolio); err != nil {
		return err
	}
	if err := AppendJSONL(LogFile(cfg.DataDir, "outcomes"), outcomes); err != nil {
		return err
	}
	if err := AppendJSONL(LogFile(cfg.DataDir, "portfolio"), []PortfolioResult{portfolio}); err != nil {
		return err
	}

	log.Printf("outcomes: scored %d candidates for %s, equal-weight close return %.3f%%",
		len(outcomes), session.Date, portfolio.All[HorizonClose].MeanPct)
	PrintOutcomes(portfolio, outcomes)
	return nil
}

// loadScanForOutcomes prefers the open run and falls back to the pre-open list.
func loadScanForOutcomes(dataDir, sessionDate string) (*ScanResult, error) {
	for _, phase := range []string{PhaseOpen, PhasePremarket} {
		path := PhaseFile(dataDir, sessionDate, phase)
		scan, err := LoadScanResult(path)
		if err != nil {
			return nil, err
		}
		if scan != nil {
			log.Printf("outcomes: scoring the %s scan from %s (%d rows)", phase, path, len(scan.Candidates))
			return scan, nil
		}
	}
	return nil, fmt.Errorf("no scan recorded for %s under %s; run the scanner on that session first",
		sessionDate, dataDir)
}

// runTrack advances the live paper book by one tick.
func runTrack(client *alpaca.Client, cfg ScannerConfig) error {
	session, err := ResolveSession(client, cfg.PremarketStartET)
	if err != nil {
		return err
	}

	scanner, err := NewScanner(client, cfg)
	if err != nil {
		return err
	}
	defer scanner.Close()

	ledger, samples, err := scanner.Track(session)
	if err != nil {
		return err
	}

	if err := SaveJSON(LedgerPath(cfg.DataDir, session.Date), ledger); err != nil {
		return err
	}
	// Two sinks: the per-session file the exporter reads, and a cumulative log.
	if err := AppendJSONL(SessionFile(cfg.DataDir, session.Date, "samples-log"), samples); err != nil {
		return err
	}
	if err := AppendJSONL(LogFile(cfg.DataDir, "samples"), samples); err != nil {
		return err
	}
	if err := appendSessionSamples(cfg.DataDir, session.Date, samples); err != nil {
		return err
	}

	log.Printf("tracker: tick %d, %d open, %d closed, %d samples",
		ledger.Ticks, ledger.Open(), len(ledger.Positions)-ledger.Open(), len(samples))
	PrintLedger(ledger, samples)
	return nil
}

// appendSessionSamples keeps the session's samples as a single JSON array,
// which is what the CSV exporter reads.
func appendSessionSamples(dataDir, date string, samples []PositionSample) error {
	if len(samples) == 0 {
		return nil
	}
	path := SessionFile(dataDir, date, "samples")

	existing, err := LoadJSON[[]PositionSample](path)
	if err != nil {
		return err
	}
	all := samples
	if existing != nil {
		all = append(*existing, samples...)
	}
	return SaveJSON(path, all)
}

// runExport flattens the collected JSON into CSV for regression work.
func runExport(cfg ScannerConfig, date string) error {
	rows, samples, err := ExportCSV(cfg.DataDir, date, cfg.ExportDir)
	if err != nil {
		return err
	}

	log.Printf("export: wrote %d candidate rows and %d sample rows to %s", rows, samples, cfg.ExportDir)
	fmt.Printf("\nExported to %s\n  candidates.csv  %d rows\n  samples.csv     %d rows\n\n",
		cfg.ExportDir, rows, samples)
	return nil
}

func main() {
	outcomes := flag.Bool("outcomes", false,
		"score a completed session's candidates instead of scanning for new ones")
	track := flag.Bool("track", false,
		"advance the live paper book by one tick: buy at the first tick, sell on target, flatten near the close")
	export := flag.Bool("export", false,
		"flatten collected JSON into candidates.csv and samples.csv")
	sweep := flag.Bool("sweep", false,
		"replay collected outcomes against a range of ATR target multiples to find the best expectancy")
	swing := flag.Bool("swing", false,
		"score scanned sessions over multi-day horizons from daily bars, tagging news-driven gaps")
	swingSweep := flag.Bool("swing-sweep", false,
		"replay swing paths across a grid of holding periods and ATR target multiples")
	date := flag.String("date", "",
		"session date YYYY-MM-DD for -outcomes, -export, -sweep, -swing and -swing-sweep (default: most recent / all)")
	flag.Parse()

	logFile := setupLogging()
	defer logFile.Close()

	log.Println("Starting process...")

	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file loaded, relying on system environment variables: %v", err)
	}

	// Create an Alpaca client with paper trading credentials
	client := alpaca.NewClient(alpaca.ClientOpts{
		APIKey:    os.Getenv("APCA_API_KEY_ID"),
		APISecret: os.Getenv("APCA_API_SECRET_KEY"),
		BaseURL:   "https://paper-api.alpaca.markets",
	})

	// Every mode starts here, so a transient 5xx would otherwise kill the run
	// before it began. A 401 is not transient and still fails immediately.
	account, err := retryTransient("account", client.GetAccount)
	if err != nil {
		log.Fatalf("Failed to get account: %v", err)
	}
	if account.TradingBlocked {
		log.Fatal("Account is currently restricted from trading.")
	}
	log.Printf("%v is available as buying power.\n", account.BuyingPower)
	log.Printf("account: equity=%v daytrading_buying_power=%v multiplier=%v",
		account.Equity, account.DaytradingBuyingPower, account.Multiplier)
	log.Printf("account: pattern_day_trader=%t daytrade_count=%d shorting_enabled=%t",
		account.PatternDayTrader, account.DaytradeCount, account.ShortingEnabled)

	config, err := loadConfig("config.json")
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}
	log.Printf("Application Loaded: %s (v%s) | Debug Mode: %t\n", config.AppName, config.Version, config.Debug)

	if !config.Scanner.Enabled {
		log.Println("Scanner disabled in config, nothing to do.")
		return
	}

	switch {
	case *export:
		if err := runExport(config.Scanner, *date); err != nil {
			log.Fatalf("Export failed: %v", err)
		}
	case *sweep:
		if err := runSweep(config.Scanner, *date); err != nil {
			log.Fatalf("Sweep failed: %v", err)
		}
	case *swingSweep:
		if err := runSwingSweep(config.Scanner, *date); err != nil {
			log.Fatalf("Swing sweep failed: %v", err)
		}
	case *swing:
		if err := runSwing(client, config.Scanner, *date); err != nil {
			log.Fatalf("Swing scoring failed: %v", err)
		}
	case *outcomes:
		if err := runOutcomes(client, config.Scanner, *date); err != nil {
			log.Fatalf("Outcome collection failed: %v", err)
		}
	case *track:
		if err := runTrack(client, config.Scanner); err != nil {
			log.Fatalf("Tracking tick failed: %v", err)
		}
	default:
		if err := runScan(client, config.Scanner); err != nil {
			log.Fatalf("Gapper scan failed: %v", err)
		}
	}
}
