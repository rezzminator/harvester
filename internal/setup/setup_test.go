package setup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/harvestpy"
	"github.com/rezzminator/harvester/internal/paths"
	"github.com/rezzminator/harvester/internal/testjail"
)

func TestMain(m *testing.M) { os.Exit(testjail.Run(m)) }

func setupFixture(t *testing.T) (config.Runtime, string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("PFM_HARVEST_ROOT", "")
	t.Setenv("HARVESTER_PYTHON_ROOT", "")
	cfg := config.Runtime{Paths: paths.Values{Home: home}, Config: config.Config{Harvester: config.DefaultHarvester()}}
	old := filepath.Join(home, ".local", "state", "pfm", "harvest-python")
	putState(t, old, "cache/pinned.tar.gz", "local bytes")
	putState(t, old, "models/model", "weights")
	putState(t, old, "env/interpreter", "old environment")
	return cfg, old, paths.PythonRoot(home)
}

func fixtureOperations(t *testing.T, old, root string, failHealth bool) operations {
	t.Helper()
	provisioned := false
	return operations{
		check: func(context.Context, string, harvestpy.Platform) (harvestpy.CheckReport, error) {
			return harvestpy.CheckReport{Healthy: provisioned && !failHealth}, nil
		},
		provision: func(_ context.Context, options harvestpy.ProvisionOptions) (harvestpy.ProvisionResult, error) {
			if !options.Offline || options.Root != root || options.Cache != filepath.Join(root, "cache") {
				t.Fatalf("options=%+v", options)
			}
			if _, err := os.Stat(filepath.Join(root, "cache", "pinned.tar.gz")); err != nil {
				t.Fatalf("local cache lost: %v", err)
			}
			if _, err := os.Stat(filepath.Join(old, "env", "interpreter")); err != nil {
				t.Fatalf("legacy removed before provisioning: %v", err)
			}
			provisioned = true
			return harvestpy.ProvisionResult{}, nil
		},
		models: func(_ context.Context, options harvestpy.OCRStageOptions) (harvestpy.OCRStaging, error) {
			if !options.Offline {
				t.Fatal("offline not propagated to models")
			}
			if _, err := os.Stat(filepath.Join(root, "models", "model")); err != nil {
				t.Fatalf("models lost: %v", err)
			}
			return harvestpy.OCRStaging{}, nil
		},
		service: func(context.Context, config.Runtime) error { return nil },
		ensure:  func(context.Context, config.Runtime) error { return nil },
	}
}

func TestSetupOfflineMigratesStateAndRemovesPythonOnlyAfterHealth(t *testing.T) {
	cfg, old, root := setupFixture(t)
	putState(t, filepath.Join(cfg.Paths.Home, ".local", "state", "pfm", "harvest-ask"), "run/source.json", "ask source")
	putState(t, filepath.Join(cfg.Paths.Home, ".professor", ".harvester-cache"), "pdf/handle.md", "persistent")
	ops := fixtureOperations(t, old, root, false)
	if err := run(t.Context(), cfg, true, io.Discard, ops); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(paths.AskRoot(cfg.Paths.Home), "run/source.json"), filepath.Join(paths.HarvesterCacheDir(cfg.Paths.Home), "pdf/handle.md")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old Python retained after health: %v", err)
	}
	if err := run(t.Context(), cfg, true, io.Discard, ops); err != nil {
		t.Fatalf("interrupted/idempotent rerun: %v", err)
	}
}

func TestSetupUnhealthyReplacementRetainsLegacyAndReruns(t *testing.T) {
	cfg, old, root := setupFixture(t)
	if err := run(t.Context(), cfg, true, io.Discard, fixtureOperations(t, old, root, true)); err == nil {
		t.Fatal("unhealthy environment accepted")
	}
	if _, err := os.Stat(filepath.Join(old, "env/interpreter")); err != nil {
		t.Fatalf("legacy lost: %v", err)
	}
	if err := run(t.Context(), cfg, true, io.Discard, fixtureOperations(t, old, root, false)); err != nil {
		t.Fatal(err)
	}
}

func TestSetupExplicitCacheAndLegacyOverrideArePreserved(t *testing.T) {
	cfg, original, root := setupFixture(t)
	override := filepath.Join(cfg.Paths.Home, "legacy-custom")
	if err := os.Rename(original, override); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PFM_HARVEST_ROOT", override)
	cfg.Config.Harvester.Cache.Dir = filepath.Join(cfg.Paths.Home, "explicit-cache")
	putState(t, cfg.Config.Harvester.Cache.Dir, "handle.md", "configured")
	legacyCache := filepath.Join(cfg.Paths.Home, ".professor", ".harvester-cache")
	putState(t, legacyCache, "item", "untouched")
	if err := run(t.Context(), cfg, true, io.Discard, fixtureOperations(t, override, root, false)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(cfg.Config.Harvester.Cache.Dir, "handle.md"), filepath.Join(legacyCache, "item")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSetupProvisionFailureRetainsLegacy(t *testing.T) {
	cfg, old, root := setupFixture(t)
	ops := fixtureOperations(t, old, root, false)
	ops.provision = func(context.Context, harvestpy.ProvisionOptions) (harvestpy.ProvisionResult, error) {
		return harvestpy.ProvisionResult{}, errors.New("offline input unavailable")
	}
	if err := run(t.Context(), cfg, true, io.Discard, ops); err == nil {
		t.Fatal("provision failure accepted")
	}
	if _, err := os.Stat(filepath.Join(old, "env/interpreter")); err != nil {
		t.Fatal(err)
	}
}

func TestSetupRejectsOverlappingPythonRoots(t *testing.T) {
	cfg, old, _ := setupFixture(t)
	t.Setenv("HARVESTER_PYTHON_ROOT", filepath.Join(old, "new"))
	if err := run(t.Context(), cfg, true, io.Discard, operations{service: func(context.Context, config.Runtime) error { return nil }}); err == nil {
		t.Fatal("nested replacement accepted")
	}
}

func TestSetupRejectsAliasedPythonRoots(t *testing.T) {
	cfg, old, _ := setupFixture(t)
	alias := filepath.Join(cfg.Paths.Home, "state-alias")
	if err := os.Symlink(filepath.Dir(old), alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARVESTER_PYTHON_ROOT", filepath.Join(alias, "harvest-python", "new"))
	if err := run(t.Context(), cfg, true, io.Discard, operations{service: func(context.Context, config.Runtime) error { return nil }}); err == nil {
		t.Fatal("replacement nested through symlink ancestor accepted")
	}
	if _, err := os.Stat(filepath.Join(old, "env/interpreter")); err != nil {
		t.Fatal(err)
	}
}

func TestSetupOCRFailureRetainsLegacy(t *testing.T) {
	cfg, old, root := setupFixture(t)
	ops := fixtureOperations(t, old, root, false)
	ops.models = func(context.Context, harvestpy.OCRStageOptions) (harvestpy.OCRStaging, error) {
		return harvestpy.OCRStaging{}, errors.New("OCR input unavailable offline")
	}
	if err := run(t.Context(), cfg, true, io.Discard, ops); err == nil {
		t.Fatal("OCR failure accepted")
	}
	if _, err := os.Stat(filepath.Join(old, "env/interpreter")); err != nil {
		t.Fatal(err)
	}
}

func TestSetupDisablesGatewayBeforeProvisionFailure(t *testing.T) {
	cfg, old, root := setupFixture(t)
	cfg.Config.Harvester.External.Enabled = false
	ops := fixtureOperations(t, old, root, false)
	disabled := false
	ops.service = func(context.Context, config.Runtime) error { disabled = true; return nil }
	ops.provision = func(context.Context, harvestpy.ProvisionOptions) (harvestpy.ProvisionResult, error) {
		if !disabled {
			t.Fatal("disabled gateway still running when provision began")
		}
		return harvestpy.ProvisionResult{}, errors.New("offline inputs absent")
	}
	if err := run(t.Context(), cfg, true, io.Discard, ops); err == nil || !disabled {
		t.Fatalf("disabled=%t error=%v", disabled, err)
	}
}

func TestSetupRefusesUnrecognizedLegacyData(t *testing.T) {
	cfg, old, root := setupFixture(t)
	putState(t, old, "operator-file", "must remain")
	if err := run(t.Context(), cfg, true, io.Discard, fixtureOperations(t, old, root, false)); err == nil {
		t.Fatal("legacy root with unrelated data accepted for removal")
	}
	if _, err := os.Stat(filepath.Join(old, "operator-file")); err != nil {
		t.Fatal(err)
	}
}

func TestSetupEnsuresDaemonLastAndPropagatesFailure(t *testing.T) {
	cfg, old, root := setupFixture(t)
	ops := fixtureOperations(t, old, root, false)
	ensured := false
	ops.ensure = func(context.Context, config.Runtime) error {
		ensured = true
		if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("daemon ensured before migration completed: %v", err)
		}
		return errors.New("daemon startup failed; see daemon.log")
	}
	if err := run(t.Context(), cfg, true, io.Discard, ops); err == nil || !ensured || !strings.Contains(err.Error(), "daemon startup failed") {
		t.Fatalf("ensure=%t err=%v", ensured, err)
	}
}
