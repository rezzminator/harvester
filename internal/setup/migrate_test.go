package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/harvest"
)

func putState(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMoveStateRenameAndIdempotentRerun(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	putState(t, source, "html/handle.md", "persistent handle")
	if err := moveState(source, target); err != nil {
		t.Fatal(err)
	}
	if err := moveState(source, target); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(target, "html/handle.md"))
	if err != nil || string(body) != "persistent handle" {
		t.Fatalf("body=%q error=%v", body, err)
	}
	if _, err := os.Lstat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy still exists: %v", err)
	}
}

func TestMoveStateAcrossFilesystemsVerifiesAndRetainsSource(t *testing.T) {
	old := renameState
	t.Cleanup(func() { renameState = old })
	renameState = func(source, target string) error {
		if filepath.Ext(source) != ".harvester-migrate" {
			return syscall.EXDEV
		}
		return os.Rename(source, target)
	}
	root := t.TempDir()
	source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	putState(t, source, "html/item.md", "verified")
	stamp := time.Unix(1234567890, 0)
	if err := os.Chtimes(filepath.Join(source, "html", "item.md"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("html/item.md", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if err := moveState(source, target); !errors.Is(err, errMigrationRetained) {
		t.Fatalf("retention notice=%v", err)
	}
	body, err := os.ReadFile(filepath.Join(target, "link"))
	if err != nil || string(body) != "verified" {
		t.Fatalf("body=%q error=%v", body, err)
	}
	info, err := os.Stat(filepath.Join(target, "html", "item.md"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode not preserved: %v %v", info, err)
	}
	if !info.ModTime().Equal(stamp) {
		t.Fatalf("cache mtime changed during migration: got %s want %s", info.ModTime(), stamp)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source not retained: %v", err)
	}
}

func TestMoveStateRejectsConflictsAndUnsafeRoots(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	putState(t, source, "item", "original")
	putState(t, target, "item", "other")
	if err := moveState(source, target); err == nil {
		t.Fatal("conflicting target accepted")
	}
	body, err := os.ReadFile(filepath.Join(source, "item"))
	if err != nil || string(body) != "original" {
		t.Fatalf("source changed: %q %v", body, err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if err := moveState(link, filepath.Join(root, "dest")); err == nil {
		t.Fatal("symlink source accepted")
	}
}

func TestMoveStateRetainsPreviouslyPublishedOrRetiredSource(t *testing.T) {
	for _, retired := range []bool{false, true} {
		t.Run(map[bool]string{false: "published", true: "deleting"}[retired], func(t *testing.T) {
			root := t.TempDir()
			source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
			putState(t, source, "keep", "preserved")
			putState(t, target, "keep", "preserved")
			original, err := os.Stat(filepath.Join(source, "keep"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(filepath.Join(target, "keep"), original.ModTime(), original.ModTime()); err != nil {
				t.Fatal(err)
			}
			if retired {
				if err := os.Rename(source, source+".harvester-migrated"); err != nil {
					t.Fatal(err)
				}
				putState(t, target, "removed-before-kill", "also preserved")
			}
			if err := moveState(source, target); !errors.Is(err, errMigrationRetained) {
				t.Fatalf("retention notice=%v", err)
			}
			retainedPath := source
			if retired {
				retainedPath += ".harvester-migrated"
			}
			if _, err := os.Stat(filepath.Join(retainedPath, "keep")); err != nil {
				t.Fatalf("previous source not retained: %v", err)
			}
		})
	}
}

func TestMoveStateCopyFailureRetainsSource(t *testing.T) {
	old := renameState
	t.Cleanup(func() { renameState = old })
	renameState = func(source, target string) error {
		if filepath.Ext(source) != ".harvester-migrate" {
			return syscall.EXDEV
		}
		return os.Rename(source, target)
	}
	root := t.TempDir()
	source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	putState(t, source, "safe", "preserved")
	if err := syscall.Mkfifo(filepath.Join(source, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveState(source, target); err == nil {
		t.Fatal("unsupported copy accepted")
	}
	if _, err := os.Stat(filepath.Join(source, "safe")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial destination published: %v", err)
	}
}

func TestMoveStateRetainsRealPersistentHarvestHandle(t *testing.T) {
	restore := harvest.StubPublicResolverForTest(func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil })
	t.Cleanup(restore)
	root := t.TempDir()
	source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	original, err := harvest.New(harvest.Options{CacheDir: source})
	if err != nil {
		t.Fatal(err)
	}
	url := "https://example.test/article"
	handle, err := original.PublicHandle(url)
	if err != nil {
		t.Fatal(err)
	}
	if err := moveState(source, target); err != nil {
		t.Fatal(err)
	}
	replacement, err := harvest.New(harvest.Options{CacheDir: target})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := replacement.ResolvePublicSource(handle)
	if err != nil || resolved != url {
		t.Fatalf("persistent handle %s resolved=%s error=%v", handle, resolved, err)
	}
}

func TestMoveStateCleansOnlyJournaledInterruptedStage(t *testing.T) {
	for _, owned := range []bool{true, false} {
		t.Run(map[bool]string{true: "owned", false: "unowned"}[owned], func(t *testing.T) {
			root := t.TempDir()
			source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
			putState(t, source, "item", "complete")
			stage := target + ".harvester-migrate"
			putState(t, stage, "item", "partial copy")
			if owned {
				body, err := json.Marshal(migrationOwner{Source: source, Target: target})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(stage+".json", body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := moveState(source, target)
			if !owned {
				if err == nil {
					t.Fatal("unowned interrupted stage removed")
				}
				if _, err := os.Stat(filepath.Join(stage, "item")); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("own interrupted stage remains: %v", err)
			}
			if _, err := os.Lstat(stage + ".json"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("own interrupted journal remains: %v", err)
			}
		})
	}
}

func TestMoveStatePreservesWritesAfterVerification(t *testing.T) {
	restore := harvest.StubPublicResolverForTest(func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil })
	t.Cleanup(restore)
	root := t.TempDir()
	source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	putState(t, source, "html/item.md", "before verification")
	original, err := harvest.New(harvest.Options{CacheDir: source})
	if err != nil {
		t.Fatal(err)
	}
	var handle string
	old := renameState
	t.Cleanup(func() { renameState = old })
	renameState = func(from, to string) error {
		if from == source {
			return syscall.EXDEV
		}
		if from == target+".harvester-migrate" {
			handle, err = original.PublicHandle("https://example.test/late-article")
			if err != nil {
				return err
			}

		}
		return os.Rename(from, to)
	}
	migrationErr := moveState(source, target)
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("late writer's source removed: %v (migration=%v)", err, migrationErr)
	}
	replacement, err := harvest.New(harvest.Options{CacheDir: target})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := replacement.ResolvePublicSource(handle)
	if err != nil || resolved != "https://example.test/late-article" {
		t.Fatalf("late persistent handle resolved=%q error=%v (migration=%v)", resolved, err, migrationErr)
	}
	for _, dir := range []string{source, target} {
		body, err := os.ReadFile(filepath.Join(dir, "html/item.md"))
		if err != nil || string(body) != "before verification" {
			t.Fatalf("late file update at %s = %q error=%v", dir, body, err)
		}
	}
	if !errors.Is(migrationErr, errMigrationRetained) {
		t.Fatalf("retained source was not reported: %v", migrationErr)
	}
}

func TestMoveStateRepeatedLateWritesRetainSourceAndBoundRetries(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	putState(t, source, "original", "preserved")
	oldRename, oldCopy := renameState, copyMigrationState
	t.Cleanup(func() { renameState, copyMigrationState = oldRename, oldCopy })
	renameState = func(string, string) error { return syscall.EXDEV }
	copies := 0
	copyMigrationState = func(from, to string) error {
		if err := copyState(from, to); err != nil {
			return err
		}
		copies++
		putState(t, source, fmt.Sprintf("late-%d", copies), "new handle")
		return nil
	}
	err := moveState(source, target)
	if err == nil || !strings.Contains(err.Error(), "keeps changing") {
		t.Fatalf("active writer not reported: %v", err)
	}
	if copies != 4 {
		t.Fatalf("copy attempts=%d want 4", copies)
	}
	for n := 1; n <= copies; n++ {
		if _, err := os.Stat(filepath.Join(source, fmt.Sprintf("late-%d", n))); err != nil {
			t.Fatalf("uncopied source lost: %v", err)
		}
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unverified target published: %v", err)
	}
}

func TestMoveStateCleanupFailureIsNotARetentionNotice(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	putState(t, source, "item", "preserved")
	oldRename := renameState
	t.Cleanup(func() { renameState = oldRename })
	renameState = func(from, to string) error {
		if from == source {
			return syscall.EXDEV
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
		return os.WriteFile(target+".harvester-migrate.json", []byte("invalid journal"), 0o600)
	}
	err := moveState(source, target)
	if err == nil || errors.Is(err, errMigrationRetained) || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("cleanup failure demoted to warning: %v", err)
	}
	if _, err := os.Stat(filepath.Join(source, "item")); err != nil {
		t.Fatalf("source lost: %v", err)
	}
}

func TestMoveStateVerifiesFinalRetryAfterWriterQuiesces(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	putState(t, source, "item", "original")
	oldRename, oldCopy := renameState, copyMigrationState
	t.Cleanup(func() { renameState, copyMigrationState = oldRename, oldCopy })
	renameState = func(from, to string) error {
		if from == source {
			return syscall.EXDEV
		}
		return os.Rename(from, to)
	}
	copies := 0
	copyMigrationState = func(from, to string) error {
		if err := copyState(from, to); err != nil {
			return err
		}
		copies++
		if copies < 4 {
			putState(t, source, "item", fmt.Sprintf("revision-%d", copies))
		}
		return nil
	}
	if err := moveState(source, target); !errors.Is(err, errMigrationRetained) {
		t.Fatalf("quiescent final copy rejected: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(target, "item"))
	if err != nil || string(body) != "revision-3" {
		t.Fatalf("final revision=%q error=%v", body, err)
	}
}

func TestMoveStateNeverOverwritesPublishedDestination(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	putState(t, source, "item", "original")
	oldRename := renameState
	t.Cleanup(func() { renameState = oldRename })
	renameState = func(from, to string) error {
		if from == source {
			return syscall.EXDEV
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
		putState(t, target, "item", "new destination writer")
		putState(t, source, "item", "late legacy writer")
		return nil
	}
	err := moveState(source, target)
	if err == nil || errors.Is(err, errMigrationRetained) {
		t.Fatalf("published conflict not reported: %v", err)
	}
	for dir, want := range map[string]string{source: "late legacy writer", target: "new destination writer"} {
		body, readErr := os.ReadFile(filepath.Join(dir, "item"))
		if readErr != nil || string(body) != want {
			t.Fatalf("writer data at %s lost: %q %v (migration=%v)", dir, body, readErr, err)
		}
	}
}
