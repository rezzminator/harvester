// Package setup provisions conversion and owns the gateway service.
package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
	root := pythonRoot(home)
	if !filepath.IsAbs(root) {
		return fmt.Errorf("Python root must be absolute")
	}
	if _, err := directory(root); err != nil {
		return err
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
		return fmt.Errorf("new conversion environment is not healthy: %w", checkErr)
	}
	if !report.Healthy {
		return fmt.Errorf("new conversion environment is not healthy")
	}
	if cfg.Config.Harvester.External.Enabled {
		if err := ops.service(ctx, cfg); err != nil {
			return fmt.Errorf("gateway service: %w", err)
		}
	}
	if err := ops.ensure(ctx, cfg); err != nil {
		return fmt.Errorf("ensure persistent harvester daemon: %w", err)
	}
	fmt.Fprintln(stdout, "setup: conversion healthy; gateway service configured; daemon ready")
	return nil
}

func directory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("refuse state root that is not a directory: %s", path)
	}
	return true, nil
}
