package harvest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicSourceAndHandleBoundariesRejectPrivateOrSymlinkNamespaces(t *testing.T) {
	setHarvestTestJail(t)
	cacheDir := t.TempDir()
	h := mustNew(t, Options{CacheDir: cacheDir})
	privateDir := filepath.Join(cacheDir, ".private")
	if err := os.MkdirAll(filepath.Join(privateDir, "handles"), 0o700); err != nil {
		t.Fatal(err)
	}
	badHandle := publicHandlePrefix + strings.Repeat("0", publicHandleHexLen)
	badPath := filepath.Join(privateDir, "handles", strings.Repeat("0", publicHandleHexLen)+".json")
	data, err := json.Marshal(publicHandleRecord{Target: "http://127.0.0.1/private"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ResolvePublicSource(badHandle); err == nil {
		t.Fatal("private stored handle target was accepted")
	}
	if _, err := h.ResolvePublicSource(filepath.Join(cacheDir, ".private", "handles", "missing.md")); err == nil {
		t.Fatal("private handle namespace was accepted as a public document")
	}

	if err := os.Mkdir(filepath.Join(cacheDir, "public"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(privateDir, filepath.Join(cacheDir, "public", "private-alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ResolvePublicSource(filepath.Join(cacheDir, "public", "private-alias")); err == nil {
		t.Fatal("public symlink alias into private cache was accepted")
	}

	if err := os.RemoveAll(filepath.Join(privateDir, "handles")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cacheDir, "public"), filepath.Join(privateDir, "handles")); err != nil {
		t.Fatal(err)
	}
	withPublicDNSForProviderTest(t)
	if _, err := h.PublicHandle("https://repository.test/public.boundary.pdf"); err == nil {
		t.Fatal("private handles symlink into public namespace accepted")
	}
}

func TestPublicCandidatesDoNotTreatUnsafeDOIURLsOrISBNLocationsAsIdentity(t *testing.T) {
	setHarvestTestJail(t)
	withPublicDNSForProviderTest(t)
	h := mustNew(t, Options{CacheDir: t.TempDir()})
	// An unsafe or non-public location gets no handle and is left out; the
	// candidates beside it keep theirs.
	for _, source := range []string{
		"ftp://doi.org/10.1234/public.boundary",
		"https://user:pass@doi.org/10.1234/public.boundary",
		"http://192.168.1.20/mirror/public.boundary.pdf",
	} {
		got, err := h.PublicCandidates([]Candidate{{URL: source, Title: "Unsafe"}, {URL: "10.1234/kept", Title: "Kept"}})
		if err != nil || len(got) != 1 || got[0].URL != "10.1234/kept" || got[0].Title != "Kept" {
			t.Fatalf("PublicCandidates(%q beside a DOI) = %#v, %v; want only the DOI candidate, no error", source, got, err)
		}
	}
	got, err := h.PublicCandidates([]Candidate{{URL: "https://repository.test/isbn/9780306406157"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !publicHandleRE.MatchString(got[0].URL) {
		t.Fatalf("ISBN-containing repository URL became identity %#v; want opaque handle", got)
	}
}
