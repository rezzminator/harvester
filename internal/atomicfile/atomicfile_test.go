package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreatePreservesAnExistingJournal(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "journal")
	if err := Create(path, []byte("owner-a"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Create(path, []byte("owner-b"), 0600); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second create=%v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "owner-a" {
		t.Fatalf("journal=%q err=%v", body, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("scratch entries=%v err=%v", entries, err)
	}
}
