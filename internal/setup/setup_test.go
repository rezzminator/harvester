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

func setupFixture(t *testing.T) (config.Runtime, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("HARVESTER_PYTHON_ROOT", "")
	cfg := config.Runtime{Paths: paths.Values{Home: home}, Config: config.Config{Harvester: config.DefaultHarvester()}}
	return cfg, paths.PythonRoot(home)
}

func fixtureOperations(t *testing.T, root string, failHealth bool) operations {
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
			provisioned = true
			return harvestpy.ProvisionResult{}, nil
		},
		models: func(_ context.Context, options harvestpy.OCRStageOptions) (harvestpy.OCRStaging, error) {
			if !options.Offline || options.Root != root {
				t.Fatalf("models options=%+v", options)
			}
			return harvestpy.OCRStaging{}, nil
		},
		service: func(context.Context, config.Runtime) error { return nil },
		ensure:  func(context.Context, config.Runtime) error { return nil },
	}
}

func TestSetupOfflineProvisionsAndRerunsIdempotently(t *testing.T) {
	cfg, root := setupFixture(t)
	ops := fixtureOperations(t, root, false)
	if err := run(t.Context(), cfg, true, io.Discard, ops); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), cfg, true, io.Discard, ops); err != nil {
		t.Fatalf("idempotent rerun: %v", err)
	}
}

func TestSetupUnhealthyReplacementFailsAndReruns(t *testing.T) {
	cfg, root := setupFixture(t)
	if err := run(t.Context(), cfg, true, io.Discard, fixtureOperations(t, root, true)); err == nil {
		t.Fatal("unhealthy environment accepted")
	}
	if err := run(t.Context(), cfg, true, io.Discard, fixtureOperations(t, root, false)); err != nil {
		t.Fatal(err)
	}
}

func TestSetupProvisionFailureFails(t *testing.T) {
	cfg, root := setupFixture(t)
	ops := fixtureOperations(t, root, false)
	ops.provision = func(context.Context, harvestpy.ProvisionOptions) (harvestpy.ProvisionResult, error) {
		return harvestpy.ProvisionResult{}, errors.New("offline input unavailable")
	}
	if err := run(t.Context(), cfg, true, io.Discard, ops); err == nil {
		t.Fatal("provision failure accepted")
	}
}

func TestSetupRefusesSymlinkedPythonRoot(t *testing.T) {
	cfg, _ := setupFixture(t)
	target := filepath.Join(cfg.Paths.Home, "elsewhere")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg.Paths.Home, "python-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARVESTER_PYTHON_ROOT", link)
	err := run(t.Context(), cfg, true, io.Discard, operations{service: func(context.Context, config.Runtime) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("symlinked Python root accepted: %v", err)
	}
}

func TestSetupOCRFailureFails(t *testing.T) {
	cfg, root := setupFixture(t)
	ops := fixtureOperations(t, root, false)
	ops.models = func(context.Context, harvestpy.OCRStageOptions) (harvestpy.OCRStaging, error) {
		return harvestpy.OCRStaging{}, errors.New("OCR input unavailable offline")
	}
	if err := run(t.Context(), cfg, true, io.Discard, ops); err == nil {
		t.Fatal("OCR failure accepted")
	}
}

func TestSetupDisablesGatewayBeforeProvisionFailure(t *testing.T) {
	cfg, root := setupFixture(t)
	cfg.Config.Harvester.External.Enabled = false
	ops := fixtureOperations(t, root, false)
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

func TestSetupPropagatesDaemonEnsureFailure(t *testing.T) {
	cfg, root := setupFixture(t)
	ops := fixtureOperations(t, root, false)
	ensured := false
	ops.ensure = func(context.Context, config.Runtime) error {
		ensured = true
		return errors.New("daemon startup failed; see daemon.log")
	}
	if err := run(t.Context(), cfg, true, io.Discard, ops); err == nil || !ensured || !strings.Contains(err.Error(), "daemon startup failed") {
		t.Fatalf("ensure=%t err=%v", ensured, err)
	}
}
