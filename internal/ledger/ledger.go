// Package ledger stores the append-only curation history used by photoscli.
package ledger

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Entry struct {
	At       time.Time `json:"at"`
	Action   string    `json:"action"`
	PhotoID  string    `json:"photoId"`
	Reason   string    `json:"reason"`
	Batch    string    `json:"batch"`
	Changed  bool      `json:"changed"`
	Verified bool      `json:"verified"`
}

type HiddenPhoto struct {
	PhotoID string    `json:"photoId"`
	Reason  string    `json:"reason"`
	Batch   string    `json:"batch"`
	At      time.Time `json:"at"`
}

type ReadResult struct {
	Photos       []HiddenPhoto `json:"photos"`
	CorruptLines int           `json:"corruptLines"`
}

func Append(path string, entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create ledger directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("protect ledger: %w", err)
	}
	for _, entry := range entries {
		data, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("encode ledger entry: %w", err)
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("append ledger entry: %w", err)
		}
	}
	return nil
}

func ReadHidden(path, reason string, since time.Duration, now time.Time) (ReadResult, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ReadResult{Photos: []HiddenPhoto{}}, nil
	}
	if err != nil {
		return ReadResult{}, fmt.Errorf("open ledger: %w", err)
	}
	defer f.Close()
	latest := map[string]Entry{}
	result := ReadResult{Photos: []HiddenPhoto{}}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil || !valid(entry) {
			result.CorruptLines++
			continue
		}
		// A hide that changed nothing means the photo was already hidden,
		// possibly by the user. It must not make the photo count as hidden
		// by curation, or a later cleanup could act on a private photo.
		if entry.Action == "hide" && !entry.Changed {
			continue
		}
		latest[entry.PhotoID] = entry
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read ledger: %w", err)
	}
	cutoff := now.Add(-since)
	for _, entry := range latest {
		if entry.Action != "hide" || !entry.Verified || (reason != "" && entry.Reason != reason) || (since > 0 && entry.At.Before(cutoff)) {
			continue
		}
		result.Photos = append(result.Photos, HiddenPhoto{PhotoID: entry.PhotoID, Reason: entry.Reason, Batch: entry.Batch, At: entry.At})
	}
	// Stable ordering makes JSON output and exclude files reproducible.
	sort.Slice(result.Photos, func(i, j int) bool { return result.Photos[i].PhotoID < result.Photos[j].PhotoID })
	return result, nil
}

// ParseSince accepts Go durations plus a whole-day suffix such as 30d.
func ParseSince(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		count, err := strconv.Atoi(days)
		if err != nil || count < 0 {
			return 0, fmt.Errorf("invalid duration %q", value)
		}
		return time.Duration(count) * 24 * time.Hour, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		return 0, fmt.Errorf("invalid duration %q", value)
	}
	return duration, nil
}

func valid(entry Entry) bool {
	return !entry.At.IsZero() && (entry.Action == "hide" || entry.Action == "unhide") && entry.PhotoID != ""
}
