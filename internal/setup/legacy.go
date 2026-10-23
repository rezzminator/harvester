package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rezzminator/harvester/internal/atomicfile"
	"github.com/rezzminator/harvester/internal/config"
)

// These retired paths are import data only; setup does not execute or depend on their former owner.
func legacyConfigRoot(home string) (string, error) {
	if path := strings.TrimSpace(os.Getenv("PFM_CONFIG")); path != "" {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("legacy PFM_CONFIG must be absolute: %q", path)
		}
		return filepath.Dir(path), nil
	}
	marker := filepath.Join(home, ".local", "share", "pfm", "install", "source-repo")
	body, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Join(home, ".professor"), nil
	}
	if err != nil {
		return "", fmt.Errorf("read legacy config location %s: %w", marker, err)
	}
	root := strings.TrimSpace(string(body))
	if !filepath.IsAbs(root) || strings.ContainsAny(root, "\r\n") {
		return "", fmt.Errorf("legacy config location %s must name one absolute directory", marker)
	}
	return filepath.Clean(root), nil
}

func importLegacyConfig(cfg config.Runtime, root string, stdout io.Writer) (config.Runtime, error) {
	source := filepath.Join(root, config.HarvesterFileName)
	body, err := os.ReadFile(source)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", source, err)
	}
	target := cfg.Config.Path
	if target == "" {
		target = config.Path(cfg.Paths.Home)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return cfg, fmt.Errorf("resolve imported config path: %w", err)
	}
	if _, err := os.Lstat(target); err == nil {
		fmt.Fprintf(stdout, "setup: legacy config retained at %s; plugin config already exists at %s\n", source, target)
		return cfg, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return cfg, fmt.Errorf("inspect plugin config %s: %w", target, err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil || document == nil {
		return cfg, fmt.Errorf("parse legacy config %s: expected JSON object (%v)", source, err)
	}
	if value, ok := document["enabled"]; ok {
		var enabled *bool
		if err := json.Unmarshal(value, &enabled); err != nil || enabled == nil {
			return cfg, fmt.Errorf("legacy config %s enabled must be boolean (%v)", source, err)
		}
		delete(document, "enabled")
	}
	preferences := filepath.Join(root, "pfm.config.json")
	if path := strings.TrimSpace(os.Getenv("PFM_CONFIG")); path != "" {
		preferences = path
	}
	if err := importLegacyPreferences(document, preferences); err != nil {
		return cfg, err
	}
	wanted, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return cfg, fmt.Errorf("encode legacy config %s: %w", source, err)
	}
	wanted = append(wanted, '\n')
	// Validate before publication; invalid legacy data never replaces a usable config.
	scratch, cleanup, err := atomicfile.WriteScratch(filepath.Dir(target), ".legacy-config-*", wanted)
	if err != nil {
		return cfg, fmt.Errorf("stage imported config: %w", err)
	}
	defer cleanup()
	imported, err := config.LoadRuntime(scratch)
	if err != nil {
		return cfg, fmt.Errorf("validate imported config from %s: %w", source, err)
	}
	if err := atomicfile.Create(target, wanted, 0600); err != nil {
		return cfg, fmt.Errorf("publish imported config %s (legacy retained at %s): %w", target, source, err)
	}
	imported.Config.Path = target
	imported.Config.Harvester.Path = target
	imported.Paths = cfg.Paths
	imported.Version = cfg.Version
	fmt.Fprintf(stdout, "setup: imported legacy config %s -> %s; legacy config retained for recovery\n", source, target)
	return imported, nil
}

func importLegacyPreferences(document map[string]json.RawMessage, path string) error {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy preferences %s: %w", path, err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(body, &legacy); err != nil || legacy == nil {
		return fmt.Errorf("parse legacy preferences %s: expected JSON object (%v)", path, err)
	}
	merge := func(target, fallback map[string]json.RawMessage, key string, fields ...string) error {
		source, err := legacyObject(fallback[key])
		if err != nil {
			return fmt.Errorf("legacy preferences %s %s: %w", path, key, err)
		}
		if len(source) == 0 {
			return nil
		}
		current, err := legacyObject(target[key])
		if err != nil {
			return fmt.Errorf("harvester preferences %s: %w", key, err)
		}
		for _, field := range fields {
			if _, exists := current[field]; !exists && len(source[field]) != 0 {
				current[field] = source[field]
			}
		}
		if len(current) == 0 {
			return nil
		}
		target[key], err = json.Marshal(current)
		return err
	}
	for _, engine := range []string{"claude", "codex"} {
		if err := merge(document, legacy, engine, "binary"); err != nil {
			return err
		}
	}
	oldAsk, err := legacyObject(legacy["ask"])
	if err != nil {
		return fmt.Errorf("legacy preferences %s ask: %w", path, err)
	}
	if len(oldAsk) == 0 {
		return nil
	}
	ask, err := legacyObject(document["ask"])
	if err != nil {
		return fmt.Errorf("harvester ask preferences: %w", err)
	}
	if _, exists := ask["engine"]; !exists && len(oldAsk["engine"]) != 0 {
		ask["engine"] = oldAsk["engine"]
	}
	for _, engine := range []string{"claude", "codex"} {
		if err := merge(ask, oldAsk, engine, "model", "effort"); err != nil {
			return err
		}
	}
	document["ask"], err = json.Marshal(ask)
	if err != nil {
		return fmt.Errorf("encode imported ask preferences: %w", err)
	}
	return nil
}

func legacyObject(body json.RawMessage) (map[string]json.RawMessage, error) {
	object := map[string]json.RawMessage{}
	if len(body) == 0 {
		return object, nil
	}
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("expected JSON object")
	}
	return object, nil
}
