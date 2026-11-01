package harvest

import (
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/rezzminator/harvester/internal/obs"
)

// TestSiteExtractorLeavesOtherSitesOnTheGenericPath: the same markup on a
// host no extractor owns goes through the injected converter.
func TestSiteExtractorLeavesOtherSitesOnTheGenericPath(t *testing.T) {
	t.Parallel()
	doc, err := html.Parse(strings.NewReader(wallFixture(t, "reddit-thread-rendered.html")))
	if err != nil {
		t.Fatal(err)
	}
	if _, extractor, ok := extractForSite("https://forum.example.test/r/examplesub/comments/abc123/", doc); ok {
		t.Fatalf("extractor %q claimed a host it does not own", extractor)
	}
	listing, err := html.Parse(strings.NewReader(`<html><body><main><h1>r/examplesub</h1></main></body></html>`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := extractForSite("https://www.reddit.com/r/examplesub/", listing); ok {
		t.Fatal("the Reddit extractor claimed a page with no thread in it")
	}
}

// TestSitePressesLoadersOnlyForARegisteredSite: the browser rung may press
// load-more buttons only on a host whose registered extractor asks for it;
// every other host, an IP and an unparsable URL are read-only.
func TestSitePressesLoadersOnlyForARegisteredSite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"https://www.reddit.com/r/examplesub/comments/abc123/example_thread/", true},
		{"https://old.reddit.com/r/examplesub/comments/abc123/example_thread/", true},
		{"https://reddit.com/r/examplesub/comments/abc123/example_thread/", true},
		{"https://WWW.Reddit.com/r/examplesub/comments/abc123/", true},
		{"https://forum.example.test/r/examplesub/comments/abc123/", false},
		{"https://93.184.216.34/r/examplesub/comments/abc123/", false},
		{"https://notreddit.com/r/examplesub/comments/abc123/", false},
		{"://not a url", false},
		{"", false},
	} {
		if got := SitePressesLoaders(tc.source); got != tc.want {
			t.Errorf("SitePressesLoaders(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// TestNoMarkupClaimedExtractorPressesLoaders: SitePressesLoaders decides by
// host alone, before any markup is read, so an extractor claiming pages by
// their markup (detect) that set pressLoaders would silently never press —
// the registry holds no such entry.
func TestNoMarkupClaimedExtractorPressesLoaders(t *testing.T) {
	t.Parallel()
	for _, extractor := range siteExtractors {
		if extractor.detect != nil && extractor.pressLoaders {
			t.Errorf("extractor %q claims pages by markup and sets pressLoaders, which SitePressesLoaders "+
				"(host only) never honours", extractor.name)
		}
	}
}

// TestSiteExtractParseFailuresAreLogged pins F13: a value this file's
// fallbacks could not parse at all — not merely one with no host — is
// logged with the value and the error, never silently swallowed as though
// nothing were there (root CLAUDE.md's "never swallow exceptions"). A
// control character makes url.Parse itself fail (net/url rejects it), which
// a bare hostless string does not.
func TestSiteExtractParseFailuresAreLogged(t *testing.T) {
	bad := "http://example.test/\nbad"
	warnLogged := func(t *testing.T, recorder *obs.Recorder, want string) {
		t.Helper()
		for _, record := range recorder.Records() {
			if record.Level != slog.LevelWarn.String() {
				continue
			}
			if _, ok := record.Field(obs.FieldErr); !ok {
				continue
			}
			if strings.Contains(record.Message, want) {
				return
			}
		}
		t.Fatalf("no WARN record naming %q logged the parse error: %s", want, recorder.Raw())
	}

	t.Run("SitePressesLoaders", func(t *testing.T) {
		_, recorder := obs.Test(t)
		if got := SitePressesLoaders(bad); got {
			t.Fatalf("SitePressesLoaders(%q) = true, want false", bad)
		}
		warnLogged(t, recorder, "loader pressing left off")
	})

	t.Run("extractForSite", func(t *testing.T) {
		_, recorder := obs.Test(t)
		doc, err := html.Parse(strings.NewReader(`<html><body></body></html>`))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, ok := extractForSite(bad, doc); ok {
			t.Fatal("extractForSite claimed an unparsable source")
		}
		warnLogged(t, recorder, "no site extractor tried")
	})

	t.Run("followForSite", func(t *testing.T) {
		ctx, recorder := obs.Test(t)
		doc, err := html.Parse(strings.NewReader(`<html><body></body></html>`))
		if err != nil {
			t.Fatal(err)
		}
		h := mustNew(t, Options{CacheDir: t.TempDir()})
		h.followForSite(ctx, bad, doc, newLoaderBudget(ctx))
		warnLogged(t, recorder, "no loaders followed")
	})

	t.Run("resolve", func(t *testing.T) {
		_, recorder := obs.Test(t)
		if got := (markdownRenderer{}).resolve(bad); got != bad {
			t.Fatalf("resolve(%q) = %q, want the href unresolved", bad, got)
		}
		warnLogged(t, recorder, "left unresolved")
	})
}

// TestMarkdownRendererKeepsRichTextStructure pins the rich-text renderer the
// per-site extractors share: headings, ordered and nested lists, hard breaks,
// quotes, code and relative links resolved against the site; literal Markdown
// characters escaped, tables with their separator row, and inline code holding
// a backtick.
func TestMarkdownRendererKeepsRichTextStructure(t *testing.T) {
	t.Parallel()
	base, err := url.Parse("https://forum.example.test/")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, fragment, want string
	}{
		{
			name: "structure",
			fragment: `<div><h2>Steps</h2><ol><li><p>Install it</p><ul><li>with <em>care</em></li></ul></li>` +
				`<li>Run <code>tool --go</code></li></ol><p>line one<br>line two</p>` +
				`<blockquote><p>quoted</p></blockquote><p>See <a href="/wiki/faq">the FAQ</a>.</p>` +
				`<button>Reply</button><pre><code>a  b
  c</code></pre></div>`,
			want: "## Steps\n\n1. Install it\n   \n   - with *care*\n2. Run `tool --go`\n\nline one  \nline two\n\n" +
				"> quoted\n\nSee [the FAQ](https://forum.example.test/wiki/faq).\n\n```\na  b\n  c\n```",
		},
		{
			name:     "escaped characters",
			fragment: `<p>*not emphasis* and [not a link]</p>`,
			want:     `\*not emphasis* and \[not a link]`,
		},
		{
			name:     "table",
			fragment: `<table><tr><th>Name</th><th>Score</th></tr><tr><td>alpha</td><td>3</td></tr></table>`,
			want:     "| Name  | Score |\n|-------|-------|\n| alpha | 3     |",
		},
		{
			name:     "links to nothing render as text",
			fragment: `<p><a href="#top">top</a> and <a href="javascript:void(0)">run</a></p>`,
			want:     "top and run",
		},
		{
			name:     "inline code holding a backtick",
			fragment: "<p>Run <code>echo `date`</code> now</p>",
			want:     "Run ``echo `date` `` now",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := html.Parse(strings.NewReader(tc.fragment))
			if err != nil {
				t.Fatal(err)
			}
			var before, after strings.Builder
			if err := html.Render(&before, doc); err != nil {
				t.Fatal(err)
			}
			got := strings.Join(markdownRenderer{base: base}.blocks(firstElement(doc, "body")), "\n\n")
			if got != tc.want {
				t.Fatalf("rendered markdown:\n%s\n--- want:\n%s", got, tc.want)
			}
			if err := html.Render(&after, doc); err != nil {
				t.Fatal(err)
			}
			if after.String() != before.String() {
				t.Fatalf("rendering changed the page:\n%s\n--- want:\n%s", after.String(), before.String())
			}
		})
	}
}
