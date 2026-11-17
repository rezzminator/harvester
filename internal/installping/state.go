package installping

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"uuid"

	"github.com/rezzminator/harvester/internal/atomicfile"
	"github.com/rezzminator/harvester/internal/obs"
	"github.com/rezzminator/harvester/internal/paths"
)

// idPattern is the contract's id rule: a lowercase UUIDv4.
var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// loadID returns the install id, creating the id file when it is missing or
// replacing it when it does not hold a valid id; created reports that this
// call wrote it.
func loadID(ctx context.Context, home string) (id string, created bool, err error) {
	path := paths.InstallID(home)
	content, err := os.ReadFile(path)
	switch {
	case err == nil:
		id = strings.TrimSpace(string(content))
		if idPattern.MatchString(id) {
			return id, false, nil
		}
		obs.Logger(ctx).Warn("installping.id.replaced", "path", path, "reason", "the id file does not hold a lowercase UUIDv4")
		id = uuid.NewV4().String()
		if err := atomicfile.Write(path, []byte(id+"\n"), 0o600); err != nil {
			return "", false, fmt.Errorf("write the install id: %w", err)
		}
		return id, true, nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", false, fmt.Errorf("read the install id: %w", err)
	}
	return createID(path)
}

// createID publishes a new id only where none exists yet: a complete scratch
// file is hard-linked to path, which fails when path exists, so two first
// starts racing each other keep the winner's id and count as one install.
func createID(path string) (string, bool, error) {
	id := uuid.NewV4().String()
	scratch, cleanup, err := atomicfile.WriteScratch(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*", []byte(id+"\n"))
	if err != nil {
		return "", false, fmt.Errorf("write the install id: %w", err)
	}
	defer cleanup()
	err = os.Link(scratch, path)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return "", false, fmt.Errorf("publish the install id: %w", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read the install id another start created: %w", err)
	}
	id = strings.TrimSpace(string(content))
	if !idPattern.MatchString(id) {
		return "", false, fmt.Errorf("the install id another start created at %s is not a lowercase UUIDv4: %q", path, id)
	}
	return id, false, nil
}

// lastDay is the UTC day (YYYY-MM-DD) of the last stored ping, or "" when none.
func lastDay(home string) (string, error) {
	content, err := os.ReadFile(paths.InstallPingDay(home))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the last ping day: %w", err)
	}
	return strings.TrimSpace(string(content)), nil
}

func recordDay(home, day string) error {
	if err := atomicfile.Write(paths.InstallPingDay(home), []byte(day+"\n"), 0o600); err != nil {
		return fmt.Errorf("record the ping day: %w", err)
	}
	return nil
}
