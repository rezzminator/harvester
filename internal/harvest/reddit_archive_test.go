package harvest

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/clock"
)

const redditArchiveHost = "arctic-shift.photon-reddit.com"

// redditArchiveSite is a Reddit that refuses every request, as it does a
// refused server address, beside an archive that answers the calls it holds.
type redditArchiveSite struct {
	mu sync.Mutex
	// answers maps an archive call (path?query) to its JSON body, or to a
	// status line ("422 {...}") for a refusal.
	answers map[string]string
	// shed counts the times a call is answered HTTP 422 before its answer, as
	// the archive sheds a call under load.
	shed  map[string]int
	asked []string
	clock *pacingClock
}

func (site *redditArchiveSite) harvester(t *testing.T) *Harvester {
	t.Helper()
	refused := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusForbidden, "text/html", "<html><body>Blocked</body></html>"), nil
	})
	archive := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != redditArchiveHost {
			return jsonResponse(request, `{}`), nil // the Wayback Machine holds no snapshot
		}
		call := request.URL.Path + "?" + request.URL.RawQuery
		site.mu.Lock()
		site.asked = append(site.asked, call)
		answer, known := site.answers[call]
		shed := site.shed[call] > 0
		if shed {
			site.shed[call]--
		}
		site.mu.Unlock()
		if shed {
			return response(request, http.StatusUnprocessableEntity, "application/json",
				`{"data":null,"error":"Timeout. Maybe slow down a bit"}`), nil
		}
		if !known {
			return response(request, http.StatusNotFound, "application/json", `{"data":null,"error":"not in the fixture"}`), nil
		}
		if status, body, refusal := strings.Cut(answer, " "); refusal && status == "422" {
			return response(request, http.StatusUnprocessableEntity, "application/json", body), nil
		}
		return jsonResponse(request, answer), nil
	})
	// 2026-11-17T00:00:00Z: the day the listing windows count back from.
	site.clock = &pacingClock{Clock: clock.Real, stepping: true, at: time.Unix(1794873600, 0)}
	return mustNew(t, Options{
		CacheDir:    t.TempDir(),
		Client:      &http.Client{Transport: refused},
		Chrome:      &http.Client{Transport: refused},
		Jina:        &http.Client{Transport: refused},
		OA:          &http.Client{Transport: archive},
		Converter:   &fakeConverter{},
		BrowserRung: browserOff(),
		Clock:       site.clock,
	})
}

const (
	redditArchivedPost = `{"data":[{"id":"1abc2d","title":"How do you keep notes &amp; plans?",` +
		`"author":"river_gauge","subreddit":"stillingwell","selftext":"I keep a paper log.\n\nIt does not scale.",` +
		`"url":"https://www.reddit.com/r/stillingwell/comments/1abc2d/how_do_you_keep_notes_plans/",` +
		`"permalink":"/r/stillingwell/comments/1abc2d/how_do_you_keep_notes_plans/","score":42,"num_comments":4,` +
		`"created_utc":1760000000,"is_self":true,"retrieved_on":1760003600,"_meta":{"retrieved_2nd_on":1760140800}}]}`
	redditArchivedReply = `{"kind":"t1","data":{"author":"[deleted]","body":"[deleted]","score":1,` +
		`"created_utc":1760004000,"retrieved_on":1760003600,"replies":""}}`
	redditArchivedBranch = `{"kind":"t1","data":{"author":"weir_keeper",` +
		`"body":"A spreadsheet per client.\n\n&gt; paper log\n\nSame here.","score":7,"created_utc":"1760003000",` +
		`"retrieved_on":1760227200,"replies":{"kind":"Listing","data":{"children":[` + redditArchivedReply +
		`,{"kind":"more","data":{"count":2,"children":["x1","x2"]}}]}}}}`
	redditArchivedTree = `{"data":[` + redditArchivedBranch + `,{"kind":"t1","data":{"author":"sluice_gate",` +
		`"body":"[removed]","score":-3,"created_utc":1760090000,"replies":""}}]}`
	// Three posts, newest first as the archive lists them; the link post has
	// the highest score, the oldest no text of its own.
	redditArchivedPosts = `{"data":[` +
		`{"id":"3ccc","title":"Weekly [thread]","author":"river_gauge","subreddit":"stillingwell",` +
		`"selftext":"Post your gauge  readings\nhere.","score":5,"num_comments":2,"created_utc":1794000000,"is_self":true},` +
		`{"id":"2bbb","title":"A flood chart","author":"[deleted]","subreddit":"stillingwell","selftext":"",` +
		`"url":"https://charts.example.test/flood","score":90,"num_comments":11,"created_utc":1793000000,"is_self":false},` +
		`{"id":"1aaa","title":"Session notes for a gauge","author":"weir_keeper","subreddit":"stillingwell",` +
		`"selftext":"[removed]","score":31,"num_comments":0,"created_utc":1792000000,"is_self":true}]}`
)

// TestRedditArchiveStandsInForARefusedRedditAddress: Reddit refuses the server
// at every live rung, and the address is answered from the archive's record —
// stored under the address asked for, by the archive rung, partial under a
// reason naming the archive and what its answer leaves out.
func TestRedditArchiveStandsInForARefusedRedditAddress(t *testing.T) {
	t.Parallel()
	const (
		threadHead = "# How do you keep notes & plans?\n\n" +
			"**Subreddit:** r/stillingwell · **Author:** u/river_gauge · **Score:** 42 · **Posted:** 2025-10-09  \n" +
			"**Thread:** https://www.reddit.com/r/stillingwell/comments/1abc2d/how_do_you_keep_notes_plans/  \n"
		threadBody = "I keep a paper log.\n\nIt does not scale.\n\n---\n\n## Comments\n\n"
		branch     = "- **u/weir_keeper** · 7 points · 2025-10-09\n" +
			"  A spreadsheet per client.\n\n  > paper log\n\n  Same here.\n" +
			"  - **[deleted]** · 1 points · 2025-10-09 · *deleted*\n"
		unexpanded = "2 replies behind 1 branch(es) the archive did not expand"
		copyOf     = "archived copy: the Arctic Shift archive's record"
		newest     = "/api/posts/search?limit=100&sort=desc&subreddit=stillingwell"
		weekly     = "**[Weekly \\[thread\\]](https://www.reddit.com/r/stillingwell/comments/3ccc/)** · u/river_gauge" +
			" · 5 points · 2 comments · 2026-11-06\n   Post your gauge readings here.\n"
		chart = "**[A flood chart](https://www.reddit.com/r/stillingwell/comments/2bbb/)** · [deleted]" +
			" · 90 points · 11 comments · 2026-10-26\n"
		notes = "**[Session notes for a gauge](https://www.reddit.com/r/stillingwell/comments/1aaa/)** · u/weir_keeper" +
			" · 31 points · 0 comments · 2026-10-14\n"
	)
	for _, row := range []struct {
		name        string
		source      string
		answers     map[string]string
		shed        map[string]int
		wantBody    string
		wantPartial string
		wantPauses  []time.Duration
	}{
		{
			name:   "a thread, read from the JSON twin of its old.reddit address",
			source: "https://old.reddit.com/r/stillingwell/comments/1abc2d/.json",
			answers: map[string]string{
				"/api/posts/ids?ids=t3_1abc2d":                     redditArchivedPost,
				"/api/comments/tree?limit=25000&link_id=t3_1abc2d": redditArchivedTree,
			},
			wantBody: threadHead +
				"**Comments:** 4 stated · 3 loaded (1 deleted, 1 removed) · gaps: " + unexpanded + "\n\n" +
				threadBody + branch + "- **u/sluice_gate** · -3 points · 2025-10-10 · *removed*\n",
			wantPartial: copyOf + " of 2025-10-12, not the live Reddit page; " + unexpanded,
		},
		{
			name:   "a thread whose comments the archive sheds once, asked again after a pause",
			source: "https://www.reddit.com/comments/1abc2d",
			answers: map[string]string{
				"/api/posts/ids?ids=t3_1abc2d":                     redditArchivedPost,
				"/api/comments/tree?limit=25000&link_id=t3_1abc2d": redditArchivedTree,
			},
			shed: map[string]int{"/api/comments/tree?limit=25000&link_id=t3_1abc2d": 1},
			wantBody: threadHead +
				"**Comments:** 4 stated · 3 loaded (1 deleted, 1 removed) · gaps: " + unexpanded + "\n\n" +
				threadBody + branch + "- **u/sluice_gate** · -3 points · 2025-10-10 · *removed*\n",
			wantPartial: copyOf + " of 2025-10-12, not the live Reddit page; " + unexpanded,
			wantPauses:  []time.Duration{2 * time.Second},
		},
		{
			name:   "one comment's branch",
			source: "https://www.reddit.com/r/stillingwell/comments/1abc2d/how_do_you_keep_notes_plans/k9z8y7x/",
			answers: map[string]string{
				"/api/posts/ids?ids=t3_1abc2d": redditArchivedPost,
				"/api/comments/tree?limit=25000&link_id=t3_1abc2d&parent_id=t1_k9z8y7x": `{"data":[` +
					redditArchivedBranch + `]}`,
			},
			wantBody: threadHead +
				"**Comments:** 4 stated · 2 loaded (1 deleted, 0 removed) · gaps: " + unexpanded +
				"; one comment's branch, as the address asks; the rest of the thread is at the thread's address\n\n" +
				threadBody + branch,
			wantPartial: copyOf + " of 2025-10-12, not the live Reddit page; " + unexpanded +
				"; one comment's branch, as the address asks; the rest of the thread is at the thread's address",
		},
		{
			name:    "a subreddit's front page, which the archive cannot rank",
			source:  "https://www.reddit.com/r/stillingwell/",
			answers: map[string]string{newest: redditArchivedPosts},
			wantBody: "# r/stillingwell — newest posts\n\n**Posts:** 3\n\n" +
				"1. " + weekly + "2. " + chart + "3. " + notes,
			wantPartial: copyOf + ", not the live Reddit page; newest posts first: the archive keeps no hot ranking",
		},
		{
			name:   "the top posts of a year, ranked here by score",
			source: "https://www.reddit.com/r/stillingwell/top/.json?t=year",
			answers: map[string]string{
				"/api/posts/search?after=1763337600&limit=100&sort=desc&subreddit=stillingwell": redditArchivedPosts,
			},
			wantBody: "# r/stillingwell — top posts of the past year\n\n**Posts:** 3\n\n" +
				"1. " + chart + "2. " + notes + "3. " + weekly,
			wantPartial: copyOf + ", not the live Reddit page; ranked here by archived score among 3 posts",
		},
		{
			name:   "a search the archive runs",
			source: "https://old.reddit.com/r/stillingwell/search.json?q=session+notes&restrict_sr=1&sort=new",
			answers: map[string]string{
				"/api/posts/search?limit=100&query=session+notes&sort=desc&subreddit=stillingwell": `{"data":[]}`,
			},
			wantBody: "# r/stillingwell — posts matching \"session notes\"\n\n**Posts:** 0\n\n",
			wantPartial: copyOf + ", not the live Reddit page; " +
				"the archive's own search of r/stillingwell, newest first",
		},
		{
			name:   "a search the archive times out on, matched here in the newest posts",
			source: "https://www.reddit.com/r/stillingwell/search/?q=Session%20GAUGE&restrict_sr=1",
			answers: map[string]string{
				"/api/posts/search?limit=100&query=Session+GAUGE&sort=desc&subreddit=stillingwell": `422 ` +
					`{"data":null,"error":"Timeout. Maybe slow down a bit"}`,
				newest: redditArchivedPosts,
			},
			wantBody: "# r/stillingwell — posts matching \"Session GAUGE\"\n\n**Posts:** 1\n\n1. " + notes,
			wantPartial: copyOf + ", not the live Reddit page; every word matched here in the 3 newest posts of " +
				"r/stillingwell, since the archive's own search did not answer " +
				"(the Reddit archive /api/posts/search: HTTP 422)",
		},
		{
			name:   "a subreddit's about page",
			source: "https://www.reddit.com/r/StillingWell/about.json",
			answers: map[string]string{
				"/api/subreddits/search?subreddit=StillingWell": `{"data":[` +
					`{"display_name":"stillingwell_meta","title":"Meta","subscribers":3},` +
					`{"display_name":"stillingwell","title":"Gauges &amp; wells","subscribers":1204,` +
					`"public_description":"River gauges.","created_utc":1574261121,"retrieved_on":1760227200,` +
					`"_meta":{"num_posts":776,"num_comments":4838,"earliest_post":1574347521}}]}`,
			},
			wantBody: "# r/stillingwell\n\n**Title:** Gauges & wells · **Subscribers:** 1204 · **Created:** 2019-11-20" +
				" · **Archived posts:** 776 · **Archived comments:** 4838 · **Earliest archived post:** 2019-11-21\n\n" +
				"River gauges.\n",
			wantPartial: copyOf + " of 2025-10-12, not the live Reddit page",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			site := &redditArchiveSite{answers: row.answers, shed: row.shed}
			result := site.harvester(t).FetchWithOptions(context.Background(), row.source, FetchOptions{Refresh: true})
			if result.Error != "" {
				t.Fatalf("the archive's record was not returned (archive calls %q): %s", site.asked, result.Error)
			}
			if !slices.Equal(site.clock.sleeps, row.wantPauses) {
				t.Fatalf("paused %v between the archive calls %q, want %v", site.clock.sleeps, site.asked, row.wantPauses)
			}
			if result.Source != row.source || result.Method != rungRedditArchive ||
				result.Rungs[len(result.Rungs)-1] != rungRedditArchive {
				t.Fatalf("source %q method %q rungs %v, want %q read by the %q rung, the last one run",
					result.Source, result.Method, result.Rungs, row.source, rungRedditArchive)
			}
			if result.Partial != row.wantPartial {
				t.Fatalf("partial:\n got %q\nwant %q", result.Partial, row.wantPartial)
			}
			stored, err := os.ReadFile(result.Path)
			if err != nil {
				t.Fatalf("read the stored artifact %s: %v", result.Path, err)
			}
			if _, body := readFrontmatter(string(stored)); body != row.wantBody {
				t.Fatalf("the stored artifact %s:\n got %q\nwant %q", result.Path, body, row.wantBody)
			}
		})
	}
}

// TestRedditArchiveFailureIsNamedBesideReddits: when the archive cannot stand
// in, the read fails as Reddit's refusal, and the message says which of the
// two the archive was — unreadable, or without a record of the address.
func TestRedditArchiveFailureIsNamedBesideReddits(t *testing.T) {
	t.Parallel()
	const (
		source  = "https://www.reddit.com/r/stillingwell/comments/1abc2d/how_do_you_keep_notes_plans/"
		refusal = "The source refused the harvester (HTTP 403 Forbidden): a bot block or an access rule, which the " +
			"harvester cannot tell apart, and it never signs in. Rungs tried: direct, chrome-impersonation, jina, " +
			"defuddle, reddit-archive, wayback. Find another copy at another URL (search_web, when configured) and " +
			"read it with read (urls), or with search_literature and read (publications) if it is a scholarly work."
		unread = " The archive of Reddit that stands in for a refused page could not be read either; " +
			"a retry later may reach it."
	)
	for _, row := range []struct {
		name       string
		answers    map[string]string
		shed       map[string]int
		wantNote   string
		wantPublic string
		wantPauses []time.Duration
	}{
		{
			name:     "the archive has no record of the thread",
			answers:  map[string]string{"/api/posts/ids?ids=t3_1abc2d": `{"data":[]}`},
			wantNote: " The archive of Reddit that stands in for a refused page holds no record of this address.",
			wantPublic: refusal +
				" The archive of Reddit that stands in for a refused page holds no record of this address.",
		},
		{
			name:    "the archive refuses the call",
			answers: map[string]string{},
			wantNote: " The archive of Reddit that stands in for a refused page could not be read either: " +
				"the Reddit archive /api/posts/ids: HTTP 404.",
			wantPublic: refusal + unread,
		},
		{
			name:    "the archive sheds the call every time it is asked",
			answers: map[string]string{"/api/posts/ids?ids=t3_1abc2d": redditArchivedPost},
			shed:    map[string]int{"/api/posts/ids?ids=t3_1abc2d": 3},
			wantNote: " The archive of Reddit that stands in for a refused page could not be read either: " +
				"the Reddit archive /api/posts/ids: HTTP 422.",
			wantPublic: refusal + unread,
			wantPauses: []time.Duration{2 * time.Second, 5 * time.Second},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			site := &redditArchiveSite{answers: row.answers, shed: row.shed}
			h := site.harvester(t)
			result := h.FetchWithOptions(context.Background(), source, FetchOptions{Refresh: true})
			if result.HTTPStatus != http.StatusForbidden || !slices.Contains(result.Rungs, rungRedditArchive) {
				t.Fatalf("status %d rungs %v, want Reddit's 403 after the %q rung ran: %s",
					result.HTTPStatus, result.Rungs, rungRedditArchive, result.Error)
			}
			if strings.Count(result.Error, row.wantNote) != 1 {
				t.Fatalf("error = %q, want it to say once %q", result.Error, row.wantNote)
			}
			published := h.PublicResult(source, result, false)
			if published.Error != row.wantPublic {
				t.Fatalf("the published failure:\n got %q\nwant %q", published.Error, row.wantPublic)
			}
			// The MCP renders a published failure a second time: that pass keeps
			// the published text whole, the rungs and the archive's sentence.
			if again := PublicFailureMessage(published); again != row.wantPublic {
				t.Fatalf("the published failure, rendered again:\n got %q\nwant %q", again, row.wantPublic)
			}
			if !slices.Equal(site.clock.sleeps, row.wantPauses) {
				t.Fatalf("paused %v between the archive calls %q, want %v", site.clock.sleeps, site.asked, row.wantPauses)
			}
		})
	}
}

// TestRedditArchiveAskReadsARedditAddress: which Reddit addresses the archive
// has a shape for, and what each asks of it.
func TestRedditArchiveAskReadsARedditAddress(t *testing.T) {
	t.Parallel()
	for source, want := range map[string]redditArchiveAsk{
		"https://www.reddit.com/r/stillingwell":      {shape: redditShapeListing, subreddit: "stillingwell"},
		"https://old.reddit.com/r/stillingwell/.rss": {shape: redditShapeListing, subreddit: "stillingwell"},
		"https://www.reddit.com/r/stillingwell/new/": {shape: redditShapeListing, subreddit: "stillingwell", order: "new"},
		"https://www.reddit.com/r/stillingwell/top/?t=month": {
			shape: redditShapeListing, subreddit: "stillingwell", order: "top", window: "month",
		},
		"https://www.reddit.com/r/stillingwell/about":                    {shape: redditShapeAbout, subreddit: "stillingwell"},
		"https://www.reddit.com/comments/1abc2d":                         {shape: redditShapeThread, post: "1abc2d"},
		"https://www.reddit.com/user/weir_keeper/comments/1abc2d/a_log/": {shape: redditShapeThread, post: "1abc2d"},
		"https://www.reddit.com/r/stillingwell/comments/1abc2d/a_log/k9z8y7x/?context=3": {
			shape: redditShapeThread, subreddit: "stillingwell", post: "1abc2d", comment: "k9z8y7x",
		},
		"https://www.reddit.com/r/stillingwell/search?q=gauge&sort=top&t=year": {
			shape: redditShapeSearch, subreddit: "stillingwell", query: "gauge", order: "top", window: "year",
		},
		// No archive shape: a search with no words or across all of Reddit, a
		// wiki, a profile, the front page, two subreddits at once, another site.
		"https://www.reddit.com/r/stillingwell/search":      {},
		"https://www.reddit.com/search?q=gauge":             {},
		"https://www.reddit.com/r/stillingwell/wiki/index":  {},
		"https://www.reddit.com/user/weir_keeper":           {},
		"https://www.reddit.com/":                           {},
		"https://www.reddit.com/r/stillingwell+weirs/":      {},
		"https://notreddit.com/r/stillingwell/":             {},
		"https://reddit.com.example.test/r/stillingwell/":   {},
		"https://www.reddit.com/r/stillingwell/comments/A!": {},
	} {
		got, ok := redditArchiveAskFor(source)
		if got != want || ok != (want.shape != "") {
			t.Errorf("redditArchiveAskFor(%q) = %+v, %t; want %+v, %t", source, got, ok, want, want.shape != "")
		}
	}
}

// TestRedditArchivePostsPagesBackFromTheOldestPostRead: a full page is
// followed by the page before its oldest post, until a short page ends the
// subreddit or the page budget ends the read with older posts left.
func TestRedditArchivePostsPagesBackFromTheOldestPostRead(t *testing.T) {
	t.Parallel()
	var full strings.Builder
	for index := range redditArchivePage {
		fmt.Fprintf(&full, `{"id":"p%d","created_utc":%d},`, index, 1794000000-index)
	}
	fullPage := `{"data":[` + strings.TrimSuffix(full.String(), ",") + `]}`
	const first = "/api/posts/search?limit=100&sort=desc&subreddit=stillingwell"
	const second = "/api/posts/search?before=1793999901&limit=100&sort=desc&subreddit=stillingwell"
	for _, row := range []struct {
		name      string
		pages     int
		wantPosts int
		wantMore  bool
		wantAsked []string
	}{
		{name: "a short page ends the read", pages: 5, wantPosts: 102, wantAsked: []string{first, second}},
		{name: "the page budget ends the read", pages: 1, wantPosts: 100, wantMore: true, wantAsked: []string{first}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			site := &redditArchiveSite{answers: map[string]string{
				first:  fullPage,
				second: `{"data":[{"id":"old1","created_utc":1793000000},{"id":"old2","created_utc":1792000000}]}`,
			}}
			posts, more, err := site.harvester(t).redditArchivePosts(
				context.Background(), "stillingwell", "", time.Time{}, row.pages)
			if err != nil {
				t.Fatalf("redditArchivePosts: %v", err)
			}
			if len(posts) != row.wantPosts || more != row.wantMore || !slices.Equal(site.asked, row.wantAsked) {
				t.Fatalf("%d posts, more %t, asked %q; want %d posts, more %t, asked %q",
					len(posts), more, site.asked, row.wantPosts, row.wantMore, row.wantAsked)
			}
		})
	}
}
