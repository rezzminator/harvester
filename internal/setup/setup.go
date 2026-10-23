// Package setup provisions conversion, migrates legacy state, and owns the gateway service.
package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/daemon"
	"github.com/rezzminator/harvester/internal/deps"
	"github.com/rezzminator/harvester/internal/harvestpy"
	"github.com/rezzminator/harvester/internal/obs"
	"github.com/rezzminator/harvester/internal/paths"
)

type operations struct {
	check     func(context.Context, string, harvestpy.Platform) (harvestpy.CheckReport, error)
	provision func(context.Context, harvestpy.ProvisionOptions) (harvestpy.ProvisionResult, error)
	models    func(context.Context, harvestpy.OCRStageOptions) (harvestpy.OCRStaging, error)
	service   func(context.Context, config.Runtime) error
	ensure    func(context.Context, config.Runtime) error
}

func Run(ctx context.Context, runtime config.Runtime, offline bool, stdout, stderr io.Writer) int {
	runner := obs.Runner(deps.RealRunner{})
	err := run(ctx, runtime, offline, stdout, operations{
		check:     harvestpy.CheckConversionEnvironment,
		provision: harvestpy.Provision,
		models:    harvestpy.StageOCRModels,
		service: func(ctx context.Context, cfg config.Runtime) error {
			return configureService(ctx, cfg, runner, platformOS())
		},
		ensure: func(ctx context.Context, cfg config.Runtime) error {
			if cfg.Config.Harvester.External.Enabled {
				_, err := daemon.Ensure(ctx, cfg)
				return err
			}
			return daemon.Refresh(ctx, cfg)
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "harvester setup: %v\n", err)
		return 1
	}
	return 0
}

func platformOS() string { return runtime.GOOS }

func pythonRoot(home string) string {
	if root := os.Getenv("HARVESTER_PYTHON_ROOT"); root != "" {
		return root
	}
	return paths.PythonRoot(home)
}

func run(ctx context.Context, cfg config.Runtime, offline bool, stdout io.Writer, ops operations) (returnErr error) {
	home := cfg.Paths.Home
	if home == "" || !filepath.IsAbs(home) {
		return fmt.Errorf("runtime home must be an absolute path")
	}
	state := paths.StateRoot(home)
	if err := os.MkdirAll(state, 0o700); err != nil {
		return fmt.Errorf("create state root: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(state, "setup.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open setup lock: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, lock.Close()) }()
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("lock setup state: %w", err)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	legacyRoot, err := legacyConfigRoot(home)
	if err != nil {
		return err
	}
	cfg, err = importLegacyConfig(cfg, legacyRoot, stdout)
	if err != nil {
		return fmt.Errorf("import legacy config: %w", err)
	}
	if !cfg.Config.Harvester.External.Enabled {
		if err := ops.service(ctx, cfg); err != nil {
			return fmt.Errorf("disable gateway service: %w", err)
		}
	}
	platform := harvestpy.Platform{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	plan, err := harvestpy.PlanConversionEnvironment(platform)
	if err != nil {
		return fmt.Errorf("conversion plan: %w", err)
	}
	if len(plan.PackageBlockers) > 0 {
		return fmt.Errorf("conversion target %s blocked-exact-lock: %v", platform, plan.PackageBlockers)
	}
	fmt.Fprintf(stdout, "setup: conversion platform=%s python=%s offline=%t\n", platform, plan.PythonVersion, offline)
	legacyState := filepath.Join(home, ".local", "state", "pfm")
	oldPython := filepath.Join(legacyState, "harvest-python")
	if override := os.Getenv("PFM_HARVEST_ROOT"); override != "" {
		oldPython = override
	}
	root := pythonRoot(home)
	if !filepath.IsAbs(root) || !filepath.IsAbs(oldPython) {
		return fmt.Errorf("Python roots must be absolute")
	}
	if err := rejectOverlappingRoots(oldPython, root); err != nil {
		return err
	}
	if _, err := directory(oldPython); err != nil {
		return err
	}
	if err := validateLegacyPython(oldPython); err != nil {
		return err
	}
	if _, err := directory(root); err != nil {
		return err
	}
	retainedLegacy := false
	migrate := func(source, target string) error {
		err := moveState(source, target)
		if errors.Is(err, errMigrationRetained) {
			retainedLegacy = true
			fmt.Fprintf(stdout, "setup: warning: %v; stop legacy writers and reconcile retained files before manual removal\n", err)
			return nil
		}
		return err
	}
	if err := migrate(filepath.Join(legacyState, "harvest-ask"), paths.AskRoot(home)); err != nil {
		return fmt.Errorf("migrate ask state: %w", err)
	}
	if cfg.Config.Harvester.Cache.Dir == "" {
		for _, name := range []string{".harvester-cache", ".cache"} {
			if err := migrate(filepath.Join(legacyRoot, name), paths.HarvesterCacheDir(home)); err != nil {
				return fmt.Errorf("migrate cache %s: %w", name, err)
			}
		}
		if cfg.Config.Harvester.External.StateDir == "" {
			if err := migrate(filepath.Join(legacyRoot, ".harvester-state"), filepath.Join(state, ".harvester-state")); err != nil {
				return fmt.Errorf("migrate gateway OAuth state: %w", err)
			}
		}
	}
	for _, name := range []string{"cache", "models"} {
		if err := migrate(filepath.Join(oldPython, name), filepath.Join(root, name)); err != nil {
			return fmt.Errorf("migrate Python %s: %w", name, err)
		}
	}
	report, checkErr := ops.check(ctx, root, platform)
	if checkErr != nil || !report.Healthy {
		fmt.Fprintf(stdout, "setup: conversion requires provisioning healthy=%t check=%v\n", report.Healthy, checkErr)
		if _, err := ops.provision(ctx, harvestpy.ProvisionOptions{Root: root, Cache: filepath.Join(root, "cache"), Platform: platform, Offline: offline}); err != nil {
			return fmt.Errorf("provision conversion (offline=%t): %w", offline, err)
		}
	}
	if _, err := ops.models(ctx, harvestpy.OCRStageOptions{Root: root, Platform: platform, Offline: offline, Announce: func(message string) { fmt.Fprintf(stdout, "setup: OCR %s\n", message) }}); err != nil {
		return fmt.Errorf("stage OCR models (offline=%t): %w", offline, err)
	}
	report, checkErr = ops.check(ctx, root, platform)
	if checkErr != nil {
		return fmt.Errorf("new conversion environment is not healthy; legacy environment retained: %w", checkErr)
	}
	if !report.Healthy {
		return fmt.Errorf("new conversion environment is not healthy; legacy environment retained")
	}
	if exists, err := directory(oldPython); err != nil {
		return err
	} else if exists && !retainedLegacy {
		if err := validateLegacyPython(oldPython); err != nil {
			return err
		}
		for _, name := range []string{"env", "env-browser"} {
			if err := os.RemoveAll(filepath.Join(oldPython, name)); err != nil {
				return fmt.Errorf("remove replaced legacy environment %s: %w", name, err)
			}
		}
		if err := os.Remove(oldPython); errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			retainedLegacy = true
			fmt.Fprintf(stdout, "setup: warning: legacy state retained at %s; stop legacy writers and reconcile retained files before manual removal\n", oldPython)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove healthy replacement's legacy Python root: %w", err)
		}
	}
	if cfg.Config.Harvester.External.Enabled {
		if err := ops.service(ctx, cfg); err != nil {
			return fmt.Errorf("gateway service: %w", err)
		}
	}
	if err := ops.ensure(ctx, cfg); err != nil {
		return fmt.Errorf("ensure persistent harvester daemon: %w", err)
	}
	if retainedLegacy {
		fmt.Fprintln(stdout, "setup: legacy sources retained; verified state copies available")
	}
	fmt.Fprintln(stdout, "setup: conversion healthy; state migration complete; gateway service configured; daemon ready")
	return nil
}

func validateLegacyPython(root string) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect legacy Python root: %w", err)
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "env", "env-browser", "cache", "models", "cache.harvester-migrated", "models.harvester-migrated":
		default:
			return fmt.Errorf("refuse removing legacy Python root with unrecognized entry %s", filepath.Join(root, entry.Name()))
		}
	}
	return nil
}

func rejectOverlappingRoots(source, target string) error {
	var err error
	source, err = physicalRoot(source)
	if err != nil {
		return err
	}
	target, err = physicalRoot(target)
	if err != nil {
		return err
	}
	for _, pair := range [][2]string{{source, target}, {target, source}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil {
			return err
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return fmt.Errorf("refuse overlapping legacy and replacement Python roots: %s and %s", source, target)
		}
	}
	return nil
}

func physicalRoot(path string) (string, error) {
	path = filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("resolve state root %s: %w", path, err)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", fmt.Errorf("resolve state root %s: %w", path, err)
	}
	resolved, err = physicalRoot(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}
