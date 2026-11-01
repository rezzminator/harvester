package harvestpy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/obs"
)

// TestLockProvisionRootSerializesTwoHolders is half of L2-F23: nothing in the
// package held a lock, so two harvester processes converged the same environment
// tree at once.
func TestLockProvisionRootSerializesTwoHolders(t *testing.T) {
	root := filepath.Join(t.TempDir(), "env-browser", "linux-amd64")
	first, err := lockProvisionRoot(context.Background(), root)
	if err != nil {
		t.Fatalf("lockProvisionRoot() error = %v", err)
	}
	second := make(chan error, 1)
	go func() {
		release, err := lockProvisionRoot(context.Background(), root)
		if err == nil {
			err = release()
		}
		second <- err
	}()
	select {
	case err := <-second:
		t.Fatalf("a second holder took the lock while the first held it (err=%v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := first(); err != nil {
		t.Fatalf("release() error = %v", err)
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("the waiting holder failed after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting holder never took the released lock")
	}
}

// TestLockProvisionRootGivesUpWithItsContext: a browser fetch waiting on a
// provision another process runs is bounded by its own context (the browser
// rung's hard deadline, an MCP client's cancel), never parked in flock(2)
// until the other provision ends.
func TestLockProvisionRootGivesUpWithItsContext(t *testing.T) {
	root := filepath.Join(t.TempDir(), "env-browser", "linux-amd64")
	first, err := lockProvisionRoot(context.Background(), root)
	if err != nil {
		t.Fatalf("lockProvisionRoot() error = %v", err)
	}
	// Released after 3 s only so a lock that ignores its context fails on the
	// assertion below rather than hanging the package.
	timer := time.AfterFunc(3*time.Second, func() { _ = first() })
	defer func() {
		if timer.Stop() {
			_ = first()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	release, err := lockProvisionRoot(ctx, root)
	if err == nil {
		_ = release()
	}
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatalf("lockProvisionRoot() under a 200ms context = %v after %s, want its deadline at once",
			err, time.Since(started))
	}
}

// TestProvisionWaitsForTheLock is the conversion-environment sibling of
// TestProvisionBrowserWaitsForTheLock below: Provision (the docling/pymupdf
// closure) took no lock at all — only the browser provisioner did — so two
// harvester processes converging the SAME conversion root at once each read a tree
// the other was halfway through replacing.
func TestProvisionWaitsForTheLock(t *testing.T) {
	root := t.TempDir()
	platform := Platform{GOOS: "linux", GOARCH: "amd64"}
	targets, cache := fakeProvisionInputs(t, root, platform)
	envRoot := filepath.Join(root, "env", platform.String())
	release, err := lockProvisionRoot(context.Background(), envRoot)
	if err != nil {
		t.Fatalf("lockProvisionRoot() error = %v", err)
	}
	reached := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, provisionErr := provisionWithTargets(context.Background(), ProvisionOptions{
			Root: root, Cache: cache, Platform: platform,
			Run: func(ctx context.Context, executable string, arguments []string, directory string) ([]byte, error) {
				select {
				case reached <- struct{}{}:
				default:
				}
				return fakeProvisionRun(t, false)(ctx, executable, arguments, directory)
			},
			Smoke: func(context.Context, Runtime) (map[string]any, error) {
				return map[string]any{
					"imports":    map[string]any{},
					"conversion": map[string]any{"ok": true},
				}, nil
			},
		}, targets)
		done <- provisionErr
	}()
	select {
	case <-reached:
		t.Fatal("Provision converged the tree while another holder had the root locked")
	case <-time.After(150 * time.Millisecond):
	}
	if err := release(); err != nil {
		t.Fatalf("release() error = %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("conversion provisioning failed after the lock was released: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("conversion provisioning never ran after the lock was released")
	}
}

// TestProvisionBrowserWaitsForTheLock: the provisioner itself must take it —
// a lock nothing calls is a lock that is not held.
func TestProvisionBrowserWaitsForTheLock(t *testing.T) {
	root := t.TempDir()
	platform := Platform{GOOS: "linux", GOARCH: "amd64"}
	targets, cache := fakeProvisionInputs(t, root, platform)
	envRoot := filepath.Join(root, "env-browser", platform.String())
	release, err := lockProvisionRoot(context.Background(), envRoot)
	if err != nil {
		t.Fatalf("lockProvisionRoot() error = %v", err)
	}
	reached := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, provisionErr := provisionBrowserWithTargets(context.Background(), ProvisionOptions{
			Root: root, Cache: cache, Platform: platform,
			Download: func(_ context.Context, url, _ string) error {
				return fmt.Errorf("unit provisioning hit the network (%s)", url)
			},
			Run: func(ctx context.Context, executable string, arguments []string, directory string) ([]byte, error) {
				select {
				case reached <- struct{}{}:
				default:
				}
				return fakeProvisionRun(t, false)(ctx, executable, arguments, directory)
			},
			Smoke: fakeBrowserSmoke(),
		}, targets)
		done <- provisionErr
	}()
	select {
	case <-reached:
		t.Fatal("ProvisionBrowser converged the tree while another holder had the root locked")
	case <-time.After(150 * time.Millisecond):
	}
	if err := release(); err != nil {
		t.Fatalf("release() error = %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("browser provisioning failed after the lock was released: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("browser provisioning never ran after the lock was released")
	}
}

// TestProvisionBrowserKeepsCurrentValidWhenTheRebuildDies is the other half of
// L2-F23: ProvisionBrowser did os.RemoveAll(final) at the path `current`
// already pointed to, so a crash — or a concurrent fetch — met a
// half-destroyed environment where a working one used to be.
func TestProvisionBrowserKeepsCurrentValidWhenTheRebuildDies(t *testing.T) {
	root := t.TempDir()
	platform := Platform{GOOS: "linux", GOARCH: "amd64"}
	targets, cache := fakeProvisionInputs(t, root, platform)
	options := func(run RunFunc, smoke SmokeFunc) ProvisionOptions {
		return ProvisionOptions{
			Root: root, Cache: cache, Platform: platform,
			Download: func(_ context.Context, url, _ string) error {
				return fmt.Errorf("unit provisioning hit the network (%s)", url)
			},
			Run:   run,
			Smoke: smoke,
		}
	}
	first, err := provisionBrowserWithTargets(
		context.Background(),
		options(fakeProvisionRun(t, false), fakeBrowserSmoke()),
		targets,
	)
	if err != nil {
		t.Fatalf("first browser provisioning failed: %v", err)
	}
	current := BrowserRuntimeRoot(root, platform)
	live, err := filepath.EvalSymlinks(current)
	if err != nil {
		t.Fatalf("current pointer does not resolve after the first provision: %v", err)
	}

	// Second run at the SAME digest — the repair path EnsureBrowser takes when
	// the live environment stops answering its smoke — and it dies partway
	// through the rebuild.
	_, err = provisionBrowserWithTargets(context.Background(), options(
		func(ctx context.Context, executable string, arguments []string, directory string) ([]byte, error) {
			if len(arguments) > 0 && arguments[0] == "sync" {
				return nil, errors.New("uv sync died mid-rebuild")
			}
			return fakeProvisionRun(t, false)(ctx, executable, arguments, directory)
		},
		func(context.Context, Runtime) (map[string]any, error) {
			return nil, errors.New("the live browser environment stopped answering its smoke")
		}), targets)
	if err == nil {
		t.Fatal("a failed rebuild reported success")
	}

	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		t.Fatalf("current pointer dangles after a failed rebuild: %v", err)
	}
	if resolved != live {
		t.Fatalf("current moved off the working environment during a failed rebuild: %s, was %s", resolved, live)
	}
	for _, relative := range []string{"environment.json", filepath.Join("project", "browser.py")} {
		if _, statErr := os.Stat(filepath.Join(current, relative)); statErr != nil {
			t.Fatalf("a failed rebuild destroyed the live environment's %s: %v", relative, statErr)
		}
	}
	if digest, inspectErr := InspectBrowser(root, platform); inspectErr != nil || digest.Digest != first.Digest {
		t.Fatalf("InspectBrowser after a failed rebuild = %v (digest %q, want %q)",
			inspectErr, digest.Digest, first.Digest)
	}
}

// TestProvisionBrowserRebuildSwapsOutTheReplacedTree: a rebuild of the live
// digest moves current to the new tree, then clears the replaced one; a
// replaced tree that cannot be removed is logged and left for the next
// rebuild, never the live environment's failure.
func TestProvisionBrowserRebuildSwapsOutTheReplacedTree(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failRemove   bool
		environments int
	}{
		{name: "the replaced tree is cleared", environments: 1},
		{name: "a replaced tree that cannot be removed stays", failRemove: true, environments: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, recorder := obs.Test(t)
			root := t.TempDir()
			platform := Platform{GOOS: "linux", GOARCH: "amd64"}
			targets, cache := fakeProvisionInputs(t, root, platform)
			build := func() (ProvisionResult, error) {
				return provisionBrowserWithTargets(ctx, ProvisionOptions{
					Root: root, Cache: cache, Platform: platform,
					Download: func(_ context.Context, url, _ string) error {
						return fmt.Errorf("unit provisioning hit the network (%s)", url)
					},
					Run: fakeProvisionRun(t, false),
					// A smoke that always refuses forces the reuse check to rebuild.
					Smoke: func(_ context.Context, runtime Runtime) (map[string]any, error) {
						if _, statErr := os.Stat(runtime.Script); statErr != nil {
							return nil, statErr
						}
						return map[string]any{"ok": true, "patchright": true, "chrome_path": "/usr/bin/google-chrome"}, nil
					},
				}, targets)
			}
			if _, err := build(); err != nil {
				t.Fatalf("first browser provisioning failed: %v", err)
			}
			envRoot := filepath.Join(root, "env-browser", platform.String())
			live, err := filepath.EvalSymlinks(BrowserRuntimeRoot(root, platform))
			if err != nil {
				t.Fatal(err)
			}
			// Break the live tree so the reuse check cannot short-circuit.
			if err := os.Remove(filepath.Join(live, "project", "browser.py")); err != nil {
				t.Fatal(err)
			}
			if tc.failRemove {
				restore := pruneRemoveAll
				pruneRemoveAll = func(path string) error {
					if path == live {
						return errors.New("permission denied")
					}
					return os.RemoveAll(path)
				}
				t.Cleanup(func() { pruneRemoveAll = restore })
			}
			if _, err := build(); err != nil {
				t.Fatalf("rebuild failed: %v", err)
			}
			resolved, err := filepath.EvalSymlinks(BrowserRuntimeRoot(root, platform))
			if err != nil {
				t.Fatalf("current pointer dangles after a rebuild: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(resolved, "project", "browser.py")); statErr != nil {
				t.Fatalf("the rebuilt environment is missing its worker: %v", statErr)
			}
			entries, err := os.ReadDir(envRoot)
			if err != nil {
				t.Fatal(err)
			}
			environments := 0
			for _, entry := range entries {
				if entry.IsDir() {
					environments++
				}
			}
			if environments != tc.environments {
				t.Fatalf("environment directories after a rebuild = %d, want %d: %v", environments, tc.environments, entries)
			}
			var leftovers []string
			for _, record := range recorder.Records() {
				if record.Message == "harvestpy.browser.leftover" {
					path, _ := record.Field("path")
					cause, _ := record.Field(obs.FieldErr)
					leftovers = append(leftovers, fmt.Sprintf("%s %v: %v", record.Level, path, cause))
				}
			}
			var want []string
			if tc.failRemove {
				want = []string{fmt.Sprintf("%s %s: permission denied", slog.LevelWarn, live)}
			}
			if !slices.Equal(leftovers, want) {
				t.Fatalf("leftover records = %q, want %q", leftovers, want)
			}
		})
	}
}

// TestConvergeKeepsItsResultWhenTheLockReleaseFails: the environment is
// published before the lock drops, so a failed unlock or close is logged and
// the converge still answers with the environment it made.
func TestConvergeKeepsItsResultWhenTheLockReleaseFails(t *testing.T) {
	for _, tc := range []struct {
		name     string
		converge func(context.Context, ProvisionOptions, map[Platform]Target) (ProvisionResult, error)
		smoke    SmokeFunc
		current  func(root string, platform Platform) string
	}{
		{
			name: "conversion", converge: provisionWithTargets, current: RuntimeRoot,
			smoke: func(context.Context, Runtime) (map[string]any, error) {
				return map[string]any{"imports": map[string]any{}, "conversion": map[string]any{"ok": true}}, nil
			},
		},
		{name: "browser", converge: provisionBrowserWithTargets, smoke: fakeBrowserSmoke(), current: BrowserRuntimeRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := provisionLock
			provisionLock = func(ctx context.Context, root string) (func() error, error) {
				release, err := lockProvisionRoot(ctx, root)
				return func() error {
					return errors.Join(release(), errors.New("close harvestpy provision lock: input/output error"))
				}, err
			}
			t.Cleanup(func() { provisionLock = restore })
			ctx, recorder := obs.Test(t)
			root := t.TempDir()
			platform := Platform{GOOS: "linux", GOARCH: "amd64"}
			targets, cache := fakeProvisionInputs(t, root, platform)
			result, err := tc.converge(ctx, ProvisionOptions{
				Root: root, Cache: cache, Platform: platform, ProcRoot: t.TempDir(),
				Download: func(_ context.Context, url, _ string) error {
					return fmt.Errorf("unit provisioning hit the network (%s)", url)
				},
				Run: fakeProvisionRun(t, false), Smoke: tc.smoke,
			}, targets)
			if err != nil {
				t.Fatalf("converge with a failing lock release: %v", err)
			}
			digest, err := ReadEnvironmentDigest(filepath.Join(tc.current(root, platform), "environment.json"))
			if err != nil || digest.Digest != result.Digest || digest.State != provisionStateReady {
				t.Fatalf("live environment = %+v, %v; want %s ready", digest, err, result.Digest)
			}
			var unlocks []string
			for _, record := range recorder.Records() {
				if record.Message == "harvestpy.provision.unlock" {
					cause, _ := record.Field(obs.FieldErr)
					unlocks = append(unlocks, fmt.Sprintf("%s %v", record.Level, cause))
				}
			}
			want := []string{slog.LevelWarn.String() + " close harvestpy provision lock: input/output error"}
			if !slices.Equal(unlocks, want) {
				t.Fatalf("unlock records = %q, want %q", unlocks, want)
			}
		})
	}
}

// fakeBrowserSmoke answers the browser provisioner's smoke without a process.
func fakeBrowserSmoke() SmokeFunc {
	return func(_ context.Context, runtime Runtime) (map[string]any, error) {
		if _, err := os.Stat(runtime.Script); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "patchright": true, "chrome_path": "/usr/bin/google-chrome"}, nil
	}
}
