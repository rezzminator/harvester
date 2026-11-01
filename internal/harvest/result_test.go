package harvest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyRungReceiptsGroupOnlyConsecutiveOACandidates(t *testing.T) {
	t.Parallel()
	rungs := []string{"direct", "oa:unpaywall", "oa:openalex", "oa:core", "wayback"}
	if got := rungsPhrase(rungs); got != "direct, oa-mirror(3 sources), wayback" {
		t.Fatalf("rungsPhrase()=%q", got)
	}
	if got := withRungs("boom", []string{"direct"}); got != "boom" {
		t.Fatalf("single-rung receipt=%q", got)
	}
	want := "boom Rungs tried: direct, wayback."
	if got := withRungs("boom", []string{"direct", "wayback"}); got != want {
		t.Fatalf("multi-rung receipt=%q, want %q", got, want)
	}
}

// TestStoredArtifactLiftsALegacyBlockOnlyFromALegacyEntry: a metadata block
// opening the body is lifted only from an entry an older harvester wrote (its
// frontmatter has no chars, which every current write carries); in a current
// entry it is the page's own text and the converter's title stands.
func TestStoredArtifactLiftsALegacyBlockOnlyFromALegacyEntry(t *testing.T) {
	t.Parallel()
	block := "**Title:** Draft\n**Source:** internal\n\n---\n\nThe body.\n"
	for _, tc := range []struct {
		name, raw, title, gaps, body string
	}{
		{
			name: "current entry",
			raw: renderFrontmatter(map[string]string{
				keySource: frontmatterSourceHarvester, "chars": "60", keyTitle: "Converter title",
			}) + block,
			title: "Converter title",
			body:  block,
		},
		{
			name: "legacy entry",
			raw: "---\nfetched_at: 2024-01-02T03:04:05Z\ntoken_count: 9\nsource: harvester\n---\n\n" +
				partialMarkerPrefix + "login wall\n\n" + block,
			title: "Draft",
			gaps:  "login wall",
			body:  "The body.\n",
		},
	} {
		meta, gaps, body := storedArtifact(tc.raw)
		if meta[keyTitle] != tc.title || gaps != tc.gaps || body != tc.body {
			t.Errorf("%s: title %q gaps %q body %q, want title %q gaps %q body %q",
				tc.name, meta[keyTitle], gaps, body, tc.title, tc.gaps, tc.body)
		}
	}
}

// TestStoredContentKeepsGapsWhenTheArtifactDoesNotRead: a re-store whose
// artifact cannot be read falls back to the inline content, which carries its
// gaps only in Partial; the content the ladder re-stores keeps them.
func TestStoredContentKeepsGapsWhenTheArtifactDoesNotRead(t *testing.T) {
	t.Parallel()
	result := Result{
		Path:    filepath.Join(t.TempDir(), "absent.md"),
		Content: "The body.\n",
		Partial: "3 of 9 comments loaded",
	}
	content := storedContent(result)
	if got := partialReason(content); got != result.Partial {
		t.Fatalf("re-stored content %q carries gaps %q, want %q", content, got, result.Partial)
	}
	if got := partialBody(content); got != "\nThe body.\n" {
		t.Fatalf("re-stored body %q, want the inline content", got)
	}
}

// TestStoreResultKeepsTheContentWhenTheCacheCannotSave: a document fetched and
// converted whole is returned whole when only its cache save fails, the
// failure named in its gaps, and the public read carries it.
func TestStoreResultKeepsTheContentWhenTheCacheCannotSave(t *testing.T) {
	setHarvestTestJail(t)
	root := t.TempDir()
	h := mustNew(t, Options{CacheDir: root, MaxInlineChars: 4})
	const source = "https://example.test/notes"
	// A file where the html cache directory goes: the save cannot create it.
	if err := os.WriteFile(filepath.Join(root, "html"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	const body = "# Gauge notes\n\nThe body.\n"
	const wantPartial = "login wall; not saved to the cache (a later read fetches it again)"
	stored := h.storeResult(source, kindHTML, "direct", withPartial(body, "login wall"), 10, 200,
		[]string{"direct"}, FetchOptions{})
	if stored.Error != "" || stored.Path != "" || stored.Content != body || stored.Partial != wantPartial {
		t.Fatalf("stored = error %q path %q content %q partial %q, want no error, no path, content %q, partial %q",
			stored.Error, stored.Path, stored.Content, stored.Partial, body, wantPartial)
	}
	public := h.PublicResult(source, stored, false)
	if public.Error != "" || public.Partial != wantPartial || public.Chars != contentChars(body) {
		t.Fatalf("public = error %q partial %q chars %d, want no error, partial %q, chars %d",
			public.Error, public.Partial, public.Chars, wantPartial, contentChars(body))
	}
}
