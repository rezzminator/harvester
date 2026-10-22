package testjail

import (
	"context"
	"github.com/rezzminator/harvester/internal/deps"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteExecutablePublishesRunnableFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture")
	if err := WriteExecutable(path, []byte("#!/bin/sh\nprintf fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	result, err := (deps.RealRunner{}).Run(context.Background(), []string{path}, deps.RunOptions{})
	if err != nil || result.ExitCode != 0 || string(result.Stdout) != "fixture" {
		t.Fatalf("fixture result=%+v err=%v", result, err)
	}
}
