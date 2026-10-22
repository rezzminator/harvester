package setup

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"syscall"

	"github.com/rezzminator/harvester/internal/atomicfile"
)

var renameState = os.Rename
var copyMigrationState = copyState

var errMigrationRetained = errors.New("legacy state retained because writers may still be active")

type entryDigest struct {
	Mode     fs.FileMode
	Hash     [sha256.Size]byte
	Link     string
	Modified int64
}

// inventory never follows links: a cache may contain links to operator files.
func inventory(root string) (map[string]entryDigest, error) {
	entries := make(map[string]entryDigest)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		digest := entryDigest{Mode: info.Mode()}
		switch {
		case info.IsDir():
		case info.Mode()&os.ModeSymlink != 0:
			digest.Link, err = os.Readlink(path)
		case info.Mode().IsRegular():
			digest.Modified = info.ModTime().UnixNano()
			var file *os.File
			file, err = os.Open(path)
			if err != nil {
				return err
			}
			hash := sha256.New()
			_, copyErr := io.Copy(hash, file)
			err = errors.Join(copyErr, file.Close())
			copy(digest.Hash[:], hash.Sum(nil))
		default:
			return fmt.Errorf("unsupported state entry %s (%s)", path, info.Mode())
		}
		if err != nil {
			return err
		}
		entries[rel] = digest
		return nil
	})
	return entries, err
}

func copyState(source, target string) error {
	return copyStateEntries(source, target, false)
}

func copyStateEntries(source, target string, exclusive bool) error {
	var directories []string
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		out := filepath.Join(target, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if exclusive {
				if exists, err := directory(out); err != nil {
					return err
				} else if exists {
					return nil
				}
			}
			if err := os.MkdirAll(out, info.Mode().Perm()); err != nil {
				return err
			}
			directories = append(directories, out)
			return os.Chmod(out, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			if exclusive {
				if _, err := os.Lstat(out); err == nil {
					return nil
				} else if !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if exclusive {
				err := os.Symlink(link, out)
				if errors.Is(err, fs.ErrExist) {
					return nil
				}
				return err
			}
			if existing, err := os.Readlink(out); err == nil && existing == link {
				return nil
			}
			if err := os.Remove(out); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return os.Symlink(link, out)
		case info.Mode().IsRegular():
			if exclusive {
				if _, err := os.Lstat(out); err == nil {
					return nil
				} else if !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
			input, err := os.Open(path)
			if err != nil {
				return err
			}
			destination := out
			if exclusive {
				scratch, err := os.MkdirTemp(filepath.Dir(out), ".harvester-migrate-*")
				if err != nil {
					return errors.Join(err, input.Close())
				}
				defer os.RemoveAll(scratch)
				destination = filepath.Join(scratch, "entry")
			}
			_, copyErr := atomicfile.WriteFrom(destination, input, info.Mode(), 0)
			if err := errors.Join(copyErr, input.Close()); err != nil {
				return err
			}
			if err := os.Chtimes(destination, info.ModTime(), info.ModTime()); err != nil {
				return fmt.Errorf("preserve migration file mtime %s: %w", out, err)
			}
			if exclusive {
				if err := os.Link(destination, out); err != nil && !errors.Is(err, fs.ErrExist) {
					return err
				}
			}
			return nil
		default:
			return fmt.Errorf("unsupported state entry %s", path)
		}
	})
	if err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := syncDirectory(directories[index]); err != nil {
			return err
		}
	}
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

// moveState renames state when possible, retaining legacy sources for verified copies on EXDEV.
func moveState(source, target string) (returnErr error) {
	if filepath.Clean(source) == filepath.Clean(target) {
		return nil
	}
	if err := rejectOverlappingRoots(source, target); err != nil {
		return err
	}
	if err := cleanupMigrationStage(source, target); err != nil {
		return err
	}
	exists, err := directory(source)
	if err != nil {
		return fmt.Errorf("inspect migration source: %w", err)
	}
	retired := source + ".harvester-migrated"
	if !exists {
		pending, err := directory(retired)
		if err != nil {
			return err
		}
		if !pending {
			return nil
		}
		old, err := inventory(retired)
		if err != nil {
			return err
		}
		current, err := inventory(target)
		if err != nil {
			return err
		}
		for name, entry := range old {
			if current[name] != entry {
				return fmt.Errorf("refuse cleanup: migrated entry differs: %s", name)
			}
		}
		return fmt.Errorf("%w: %s (verified copy at %s)", errMigrationRetained, retired, target)
	}
	if pending, err := directory(retired); err != nil || pending {
		if err != nil {
			return err
		}
		return fmt.Errorf("migration retirement path already exists: %s", retired)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	targetExists, err := directory(target)
	if err != nil {
		return err
	}
	if !targetExists {
		err := renameState(source, target)
		if err == nil {
			return errors.Join(syncDirectory(filepath.Dir(target)), syncDirectory(filepath.Dir(source)))
		}
		if !errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("move %s to %s: %w", source, target, err)
		}
		stage := target + ".harvester-migrate"
		marker, err := json.Marshal(migrationOwner{Source: source, Target: target})
		if err != nil {
			return fmt.Errorf("encode migration ownership: %w", err)
		}
		if err := atomicfile.Create(stage+".json", marker, 0o600); err != nil {
			return fmt.Errorf("record migration ownership: %w", err)
		}
		defer func() {
			if cleanupErr := cleanupMigrationStage(source, target); cleanupErr != nil {
				if errors.Is(returnErr, errMigrationRetained) {
					returnErr = fmt.Errorf("migration source retained at %s; stage cleanup failed: %w", source, cleanupErr)
				} else {
					returnErr = errors.Join(returnErr, cleanupErr)
				}
			}
		}()
		if err := os.Mkdir(stage, 0o700); err != nil {
			return fmt.Errorf("create migration staging: %w", err)
		}
		if err := copyMigrationState(source, stage); err != nil {
			return fmt.Errorf("copy migration: %w", err)
		}
		if err := verifyMigrationCopy(source, stage); err != nil {
			return err
		}
		if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("migration destination appeared before publication: %s (%v)", target, err)
		}
		if err := renameState(stage, target); err != nil {
			return fmt.Errorf("publish migrated state: %w", err)
		}
		if err := syncDirectory(filepath.Dir(target)); err != nil {
			return err
		}
	} else {
		before, err := inventory(source)
		if err != nil {
			return err
		}
		after, err := inventory(target)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, after) {
			return fmt.Errorf("refuse conflicting migration destination: %s", target)
		}
	}
	// A writer may retain an open file after any inventory, so copying never authorizes unlinking the source.
	if err := verifyPublishedMigrationCopy(source, target); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s (verified copy at %s)", errMigrationRetained, source, target)
}

func verifyMigrationCopy(source, target string) error {
	for attempt := 0; attempt <= 3; attempt++ {
		before, err := inventory(source)
		if err != nil {
			return err
		}
		after, err := inventory(target)
		if err != nil {
			return err
		}
		if reflect.DeepEqual(before, after) {
			return nil
		}
		if attempt == 3 {
			break
		}
		if err := copyMigrationState(source, target); err != nil {
			return fmt.Errorf("carry late migration writes (source retained at %s): %w", source, err)
		}
	}
	return fmt.Errorf("legacy state keeps changing; source retained at %s; stop legacy writers and retry", source)
}

// Published state may already have readers and writers: only add complete, absent entries.
func verifyPublishedMigrationCopy(source, target string) error {
	for attempt := 0; attempt <= 3; attempt++ {
		before, err := inventory(source)
		if err != nil {
			return err
		}
		after, err := inventory(target)
		if err != nil {
			return err
		}
		missing := false
		for name, entry := range before {
			if current, exists := after[name]; !exists {
				missing = true
			} else if current != entry {
				return fmt.Errorf("published migration conflict at %s; source retained at %s; stop writers and reconcile", name, source)
			}
		}
		if !missing {
			return nil
		}
		if attempt == 3 {
			break
		}
		if err := copyStateEntries(source, target, true); err != nil {
			return fmt.Errorf("carry late migration writes (source retained at %s): %w", source, err)
		}
	}
	return fmt.Errorf("legacy state keeps changing; source retained at %s; stop legacy writers and retry", source)
}

type migrationOwner struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// Only a journal naming this exact move authorizes removing its interrupted stage.
func cleanupMigrationStage(source, target string) error {
	stage := target + ".harvester-migrate"
	journal := stage + ".json"
	info, err := os.Lstat(journal)
	if errors.Is(err, fs.ErrNotExist) {
		if _, stageErr := os.Lstat(stage); !errors.Is(stageErr, fs.ErrNotExist) {
			return fmt.Errorf("refuse unowned migration stage %s (%v)", stage, stageErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect migration journal: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return fmt.Errorf("refuse invalid migration journal %s", journal)
	}
	body, err := os.ReadFile(journal)
	if err != nil {
		return fmt.Errorf("read migration journal: %w", err)
	}
	var owner migrationOwner
	if err := json.Unmarshal(body, &owner); err != nil {
		return fmt.Errorf("decode migration journal: %w", err)
	}
	if owner.Source != source || owner.Target != target {
		return fmt.Errorf("refuse migration journal belonging to another move: %s", journal)
	}
	if _, err := directory(stage); err != nil {
		return err
	}
	if err := os.RemoveAll(stage); err != nil {
		return fmt.Errorf("remove interrupted migration stage: %w", err)
	}
	if err := os.Remove(journal); err != nil {
		return fmt.Errorf("remove migration journal: %w", err)
	}
	return syncDirectory(filepath.Dir(target))
}

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open migration parent: %w", err)
	}
	return errors.Join(file.Sync(), file.Close())
}
