// Package searxng is the harvester's own web-search backend: a SearXNG
// container it provisions, or an existing one it adopts, when the operator has
// configured no search backend. Docker runs through deps.Runner. The backend's
// facts live in a record under the harvester's state root; the harvester never
// writes harvester.config.json for it.
package searxng

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/paths"
)

// Pin is the image the managed backend runs, by digest, so a moved tag never
// changes what runs (SearXNG 2026.9.13).
const Pin = "searxng/searxng@sha256:e0e0557627cc3e46cc82d8c23bb3a3a74eb203ae42f73142db555551af336096"

const (
	// DefaultName is the managed container's name; a container already named so is adopted.
	DefaultName = "harvester-searxng"
	// DefaultPort is the loopback port tried first; a taken one yields another free loopback port.
	DefaultPort = 8888
	// ManagedLabel marks a container this package created; an adopted one lacks it.
	ManagedLabel = "harvester.managed"
	// EnvName and EnvPort give a second instance its own container name and
	// first port, for a hand proof under a scratch state root; they are not
	// operator configuration.
	EnvName = "HARVESTER_SEARXNG_CONTAINER"
	EnvPort = "HARVESTER_SEARXNG_PORT"

	containerPort = "8080/tcp"
	maxRecord     = 64 << 10
)

// Hint is what every surface that finds no search backend tells the operator.
const Hint = "for web search, run harvester setup with Docker running, or set search.searxngURL or search.braveApiKey in " +
	config.HarvesterFileName

// FailedHint is what follows an ensure that reached Docker and failed: its
// detail names the cause.
const FailedHint = "fix that and run harvester setup again, or set search.searxngURL or search.braveApiKey in " +
	config.HarvesterFileName

// Record is the managed backend as last ensured. Ready is written true only
// after a JSON search answered on URL.
type Record struct {
	URL       string    `json:"url"`
	Container string    `json:"container"`
	Image     string    `json:"image"`
	Adopted   bool      `json:"adopted"`
	Settings  string    `json:"settings,omitempty"`
	Ready     bool      `json:"ready"`
	Checked   time.Time `json:"checked"`
	Error     string    `json:"error,omitempty"`
}

// Dir holds the record, the generated settings and the ensure lock.
func Dir(home string) string { return filepath.Join(paths.StateRoot(home), "search") }

// RecordPath is the managed backend's record.
func RecordPath(home string) string { return filepath.Join(Dir(home), "managed.json") }

// SettingsPath is the generated settings.yml a created container mounts.
func SettingsPath(home string) string { return filepath.Join(Dir(home), "settings.yml") }

// ReadRecord reads the managed record; an absent one is fs.ErrNotExist.
func ReadRecord(home string) (Record, error) {
	if home == "" || !filepath.IsAbs(home) {
		return Record{}, fmt.Errorf("read search record: home must be an absolute path, got %q", home)
	}
	path := RecordPath(home)
	file, err := os.Open(path)
	if err != nil {
		return Record{}, fmt.Errorf("read search record %s: %w", path, err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxRecord+1))
	if err != nil {
		return Record{}, fmt.Errorf("read search record %s: %w", path, err)
	}
	if len(body) > maxRecord {
		return Record{}, fmt.Errorf("search record %s is over %d bytes", path, maxRecord)
	}
	var record Record
	if err := json.Unmarshal(body, &record); err != nil {
		return Record{}, fmt.Errorf("decode search record %s: %w", path, err)
	}
	return record, nil
}

func writeRecord(home string, record Record) error {
	dir := Dir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create search state %s: %w", dir, err)
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode search record: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".managed-*.json")
	if err != nil {
		return fmt.Errorf("write search record: %w", err)
	}
	_, writeErr := temp.Write(append(body, '\n'))
	if closeErr := temp.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Chmod(temp.Name(), 0o600)
	}
	if writeErr == nil {
		writeErr = os.Rename(temp.Name(), RecordPath(home))
	}
	if writeErr != nil {
		return errors.Join(fmt.Errorf("write search record %s: %w", RecordPath(home), writeErr), os.Remove(temp.Name()))
	}
	return nil
}

// Source names where the search backend in effect comes from.
type Source string

const (
	SourceDisabled Source = "disabled"
	SourceOperator Source = "search.searxngURL"
	SourceBrave    Source = "search.braveApiKey"
	SourceOptOut   Source = "managed-off"
	SourceManaged  Source = "managed"
	SourceAdopted  Source = "adopted"
	SourceNone     Source = "none"
)

// Backend is the search backend in effect. URL is the SearXNG origin searches
// use, empty for Brave alone and when no backend answers.
type Backend struct {
	URL    string
	Source Source
	Detail string
	Record *Record
}

// Wanted reports whether the managed backend applies: search on, neither
// operator backend set, and search.managed not false.
func Wanted(search config.HarvesterSearch) bool {
	return search.Enabled && search.SearXNGURL == "" && search.BraveAPIKey == "" && search.Managed
}

// Resolve is the one resolution of the backend in effect: the operator's
// search.searxngURL wins, then search.braveApiKey, then a ready managed record.
func Resolve(home string, search config.HarvesterSearch) Backend {
	switch {
	case !search.Enabled:
		return Backend{Source: SourceDisabled, Detail: "search.enabled is false"}
	case search.SearXNGURL != "":
		return Backend{URL: search.SearXNGURL, Source: SourceOperator}
	case search.BraveAPIKey != "":
		return Backend{Source: SourceBrave}
	case !search.Managed:
		return Backend{Source: SourceOptOut, Detail: "search.managed is false"}
	}
	record, err := ReadRecord(home)
	if errors.Is(err, fs.ErrNotExist) {
		return Backend{Source: SourceNone, Detail: "no managed backend yet"}
	}
	if err != nil {
		return Backend{Source: SourceNone, Detail: err.Error()}
	}
	if !record.Ready || record.URL == "" {
		detail := "the managed backend is not ready"
		if record.Error != "" {
			detail += ": " + record.Error
		}
		return Backend{Source: SourceNone, Detail: detail, Record: &record}
	}
	source := SourceManaged
	if record.Adopted {
		source = SourceAdopted
	}
	return Backend{URL: record.URL, Source: source, Record: &record}
}

// Describe names the backend in effect and where it comes from, in one phrase.
func (b Backend) Describe() string {
	switch b.Source {
	case SourceOperator:
		return "search.searxngURL " + b.URL
	case SourceBrave:
		return "Brave (search.braveApiKey)"
	case SourceManaged, SourceAdopted:
		return fmt.Sprintf("%s SearXNG %s (container %s)", b.Source, b.URL, b.Record.Container)
	}
	return "none (" + b.Detail + ")"
}

// Origin is the SearXNG origin a runtime searches: the operator's, else a
// ready managed backend's, else empty.
func Origin(home string, search config.HarvesterSearch) string {
	return Resolve(home, search).URL
}
