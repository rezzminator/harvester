package setup

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
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

func TestMoveStateAcrossFilesystemsVerifiesAndPreservesLinks(t *testing.T) {
	old := renameState
	t.Cleanup(func() { renameState = old })
	renameState = func(string, string) error { return syscall.EXDEV }
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
	if err := moveState(source, target); err != nil {
		t.Fatal(err)
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
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source not removed: %v", err)
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

func TestMoveStateResumesAfterPublishOrInterruptedDeletion(t *testing.T) {
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
			if err := moveState(source, target); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(source + ".harvester-migrated"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("retirement not cleaned: %v", err)
			}
		})
	}
}

func TestMoveStateCopyFailureRetainsSource(t *testing.T) {
	old := renameState
	t.Cleanup(func() { renameState = old })
	renameState = func(string, string) error { return syscall.EXDEV }
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
