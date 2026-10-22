// Package config loads the standalone harvester's operator settings.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rezzminator/harvester/internal/engine"
	"github.com/rezzminator/harvester/internal/paths"
)

type Claude struct{ Binary string }
type Codex struct{ Binary string }
type EnginePrefs struct {
	Model  string
	Effort string
}
type AskConfig struct {
	Engine engine.ID
	Prefs  map[engine.ID]EnginePrefs
}

func (config AskConfig) PrefsFor(id engine.ID) EnginePrefs { return config.Prefs[id] }

type Config struct {
	Path      string
	Harvester HarvesterConfig
	Ask       AskConfig
	Claude    Claude
	Codex     Codex
}
type Runtime struct {
	Config  Config
	Paths   paths.Values
	Version string
}

func (config Config) DefaultEngine() (engine.ID, error) {
	id := config.Ask.Engine
	if id == "" {
		id = engine.Codex
	}
	if _, err := engine.Lookup(id); err != nil {
		return "", err
	}
	return id, nil
}

type rawBinary struct {
	Binary *string `json:"binary,omitempty"`
}
type rawEngine struct {
	Model  *string `json:"model,omitempty"`
	Effort *string `json:"effort,omitempty"`
}
type rawAsk struct {
	Engine *string
	Prefs  map[engine.ID]rawEngine
}

func (raw *rawAsk) UnmarshalJSON(content []byte) error {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(content, &values); err != nil {
		return err
	}
	raw.Prefs = make(map[engine.ID]rawEngine)
	for key, value := range values {
		if key == "engine" {
			var name string
			if err := json.Unmarshal(value, &name); err != nil {
				return fmt.Errorf("ask.engine: %w", err)
			}
			raw.Engine = &name
			continue
		}
		id, err := engine.Parse(key)
		if err != nil {
			return fmt.Errorf("ask.%s: %w", key, err)
		}
		if id != engine.Claude && id != engine.Codex {
			return fmt.Errorf("ask.%s does not support ask", key)
		}
		var prefs rawEngine
		if err := decodeStrict(value, &prefs); err != nil {
			return fmt.Errorf("ask.%s: %w", key, err)
		}
		raw.Prefs[id] = prefs
	}
	return nil
}
func loadAsk(result *Config, raw rawHarvester) error {
	if raw.Ask != nil {
		if raw.Ask.Engine != nil {
			id, err := engine.Parse(*raw.Ask.Engine)
			if err != nil {
				return fmt.Errorf("ask.engine: %w", err)
			}
			if id != engine.Claude && id != engine.Codex {
				return fmt.Errorf("ask.engine %s does not support ask", *raw.Ask.Engine)
			}
			result.Ask.Engine = id
		}
		for id, prefs := range raw.Ask.Prefs {
			effective := result.Ask.Prefs[id]
			if prefs.Model != nil {
				effective.Model = strings.TrimSpace(*prefs.Model)
				if effective.Model == "" {
					return fmt.Errorf("ask.%s.model must not be empty", engine.MustLookup(id).LongName)
				}
			}
			if prefs.Effort != nil {
				effective.Effort = strings.TrimSpace(*prefs.Effort)
			}
			result.Ask.Prefs[id] = effective
		}
	}
	for _, entry := range []struct {
		name   string
		raw    *rawBinary
		target *string
	}{{"claude", raw.Claude, &result.Claude.Binary}, {"codex", raw.Codex, &result.Codex.Binary}} {
		if entry.raw == nil || entry.raw.Binary == nil {
			continue
		}
		value := strings.TrimSpace(*entry.raw.Binary)
		if value == "" || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%s.binary must be a non-empty command", entry.name)
		}
		*entry.target = value
	}
	return nil
}

// Path resolves the operator override, then the XDG config location.
func Path(home string) string {
	if path := os.Getenv("HARVESTER_CONFIG"); path != "" {
		return path
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "harvester", HarvesterFileName)
}
func LoadRuntime(path string) (Runtime, error) {
	home, err := paths.Home()
	if err != nil {
		return Runtime{}, fmt.Errorf("resolve home: %w", err)
	}
	if path == "" {
		path = Path(home)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return Runtime{}, fmt.Errorf("resolve config path: %w", err)
	}
	config := Config{Path: path, Harvester: DefaultHarvester(), Claude: Claude{Binary: "claude"}, Codex: Codex{Binary: "codex"}, Ask: AskConfig{Engine: engine.Codex, Prefs: map[engine.ID]EnginePrefs{engine.Claude: {Model: "claude-haiku-4-5", Effort: "low"}, engine.Codex: {Model: "gpt-5.6-luna", Effort: "low"}}}}
	if err := loadHarvester(&config, home); err != nil {
		return Runtime{}, err
	}
	return Runtime{Config: config, Paths: paths.Values{Home: home}}, nil
}
func decodeStrict(content []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
func configJSONError(path string, err error, contentSize ...int64) error {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Errorf("parse config %s at byte %d: %w", path, syntax.Offset, err)
	}
	var kind *json.UnmarshalTypeError
	if errors.As(err, &kind) {
		return fmt.Errorf("parse config %s at byte %d: %w", path, kind.Offset, err)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) && len(contentSize) > 0 {
		return fmt.Errorf("parse config %s at byte %d: %w", path, contentSize[0], err)
	}
	return fmt.Errorf("parse config %s: %w", path, err)
}
func expandHomePath(value, home string) (string, error) {
	if strings.ContainsRune(value, '\x00') {
		return "", errors.New("must not contain NUL")
	}
	switch {
	case value == "~" || value == "$HOME":
		value = home
	case strings.HasPrefix(value, "~/"):
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	case strings.HasPrefix(value, "$HOME/"):
		value = filepath.Join(home, strings.TrimPrefix(value, "$HOME/"))
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("must be absolute or start with ~/ or $HOME/, got %q", value)
	}
	return filepath.Clean(value), nil
}
