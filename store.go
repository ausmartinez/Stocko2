package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Observation is one candidate row in the append-only log, flattened with its
// run metadata so each line stands alone as a training example.
type Observation struct {
	RunAt       string `json:"run_at"`
	SessionDate string `json:"session_date"`
	Phase       string `json:"phase"`
	Feed        string `json:"feed"`
	GapCandidate
}

// PhaseFile is where a single run's full result is kept.
func PhaseFile(dataDir, sessionDate, phase string) string {
	return filepath.Join(dataDir, sessionDate, phase+".json")
}

// SessionFile is any other per-session artefact, e.g. tracks or portfolio.
func SessionFile(dataDir, sessionDate, name string) string {
	return filepath.Join(dataDir, sessionDate, name+".json")
}

// LogFile is an append-only log at the top of the data directory.
func LogFile(dataDir, name string) string {
	return filepath.Join(dataDir, name+".jsonl")
}

// LoadJSON reads a JSON file into T, returning nil when the file is absent.
func LoadJSON[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &v, nil
}

// SaveJSON writes v as pretty JSON, creating parent directories.
func SaveJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// AppendJSONL appends one JSON object per line.
func AppendJSONL[T any](path string, rows []T) error {
	if len(rows) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return fmt.Errorf("writing to %s: %w", path, err)
		}
	}
	return nil
}

// ObservationsFile is the append-only log across all sessions.
func ObservationsFile(dataDir string) string {
	return filepath.Join(dataDir, "observations.jsonl")
}

// LoadScanResult reads a previously written result, returning nil when absent.
func LoadScanResult(path string) (*ScanResult, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var result ScanResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &result, nil
}

// SaveScanResult writes one run's full result.
func SaveScanResult(path string, result *ScanResult) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(result, "", "    ")
	if err != nil {
		return fmt.Errorf("encoding result: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// AppendObservations appends one JSON line per candidate, so the log grows
// across sessions and can be read straight into a dataframe.
func AppendObservations(path string, result *ScanResult) error {
	if len(result.Candidates) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, c := range result.Candidates {
		obs := Observation{
			RunAt:        result.GeneratedAt,
			SessionDate:  result.SessionDate,
			Phase:        result.Phase,
			Feed:         result.Feed,
			GapCandidate: c,
		}
		if err := enc.Encode(obs); err != nil {
			return fmt.Errorf("writing observation for %s: %w", c.Symbol, err)
		}
	}
	return nil
}
