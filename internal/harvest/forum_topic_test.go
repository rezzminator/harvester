package harvest

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/clock"
)

// The topic pages below are hand-written, in the shape of markup captured
// from a phpBB 3.3 board (body#phpbb.section-viewtopic, div.pagination with
// its "N posts" text and rel="next" arrow, div.post#p{id} holding p.author
// and div.content) and an Invision Community 5 board (body data-pageApp /
// data-pageController / data-id, link rel="next" and rel="canonical",
// ul.ipsPagination data-pages, article#elComment_{id} with its profile link,
// time and data-role="commentContent"); the hosts are placeholders.

const (
	forumPhpBBHost    = "forum.example"
	forumInvisionHost = "board.example"
)

// forumPhpBBURL is page (1-based) of the placeholder phpBB topic, as its
// next link reads once its session id is dropped.
func forumPhpBBURL(page int) string {
	if page == 1 {
		return "https://" + forumPhpBBHost + "/viewtopic.php?t=104"
	}
	return fmt.Sprintf("https://%s/viewtopic.php?start=%d&t=104", forumPhpBBHost, (page-1)*2)
}

// forumPhpBBPage is page of a phpBB topic of pages pages, two posts a page.
func forumPhpBBPage(page, pages int) string {
	var pager strings.Builder
	fmt.Fprintf(&pager, "<div class=\"pagination\">\n\t\t\t%d posts\n<ul>\n", pages*2)
	for number := 1; number <= pages; number++ {
		if number == page {
			fmt.Fprintf(&pager, "<li class=\"active\"><span>%d</span></li>\n", number)
			continue
		}
		fmt.Fprintf(&pager, "<li><a class=\"button\" href=\"./viewtopic.php?t=104&amp;sid=0f3a&amp;start=%d\">%d</a></li>\n",
			(number-1)*2, number)
	}
	if page < pages {
		fmt.Fprintf(&pager, "<li class=\"arrow next\"><a class=\"button button-icon-only\" "+
			"href=\"./viewtopic.php?t=104&amp;sid=0f3a&amp;start=%d\" rel=\"next\" role=\"button\">"+
			"<span class=\"sr-only\">Next</span></a></li>\n", page*2)
	}
	pager.WriteString("</ul>\n</div>\n")
	var posts strings.Builder
	for index := 1; index <= 2; index++ {
		number := (page-1)*2 + index
		fmt.Fprintf(&posts, `<div id="p%d" class="post has-profile bg2"><div class="inner">
<dl class="postprofile"><dt><a href="./memberlist.php?mode=viewprofile&amp;u=%d" class="username">member_%d</a></dt></dl>
<div class="postbody"><div id="post_content%d"><h3><a href="./viewtopic.php?p=%d#p%d">Re: Placeholder topic</a></h3>
<p class="author"><span class="responsive-hide">by <strong><a href="./memberlist.php?mode=viewprofile&amp;u=%d" class="username">member_%d</a></strong> &raquo; </span><time datetime="2026-03-%02dT10:00:00+00:00">%d. Mar 2026</time></p>
<div class="content">Post %d of the placeholder topic.</div>
</div></div></div></div>
`, 1000+number, number, number, 1000+number, 1000+number, 1000+number, number, number,
			forumDay(number), forumDay(number), number)
	}
	canonical := forumPhpBBURL(page)
	if page > 1 {
		canonical = fmt.Sprintf("https://%s/viewtopic.php?t=104&amp;start=%d", forumPhpBBHost, (page-1)*2)
	}
	return `<!DOCTYPE html><html><head><title>Placeholder topic - Placeholder Board</title>
<link rel="canonical" href="` + canonical + `"></head>
<body id="phpbb" class="nojs notouch section-viewtopic ltr ">
<h2 class="topic-title"><a href="./viewtopic.php?t=104">Placeholder topic</a></h2>
` + pager.String() + posts.String() + pager.String() + `</body></html>`
}

// forumDay is the day of March a placeholder post is dated: one a post,
// cycling within the month.
func forumDay(number int) int { return (number-1)%28 + 1 }

// forumInvisionURL is page (1-based) of the placeholder Invision topic.
func forumInvisionURL(page int) string {
	if page == 1 {
		return "https://" + forumInvisionHost + "/index.php?/topic/77-placeholder-topic/"
	}
	return fmt.Sprintf("https://%s/index.php?/topic/77-placeholder-topic/page/%d/", forumInvisionHost, page)
}

// forumInvisionPage is page of an Invision topic of pages pages, two posts a
// page.
func forumInvisionPage(page, pages int) string {
	links := `<link rel="canonical" href="` + forumInvisionURL(page) + `">`
	if page < pages {
		links += "\n" + `<link rel="next" href="` + forumInvisionURL(page+1) + `">`
	}
	var posts strings.Builder
	for index := 1; index <= 2; index++ {
		number := (page-1)*2 + index
		fmt.Fprintf(&posts, `<article id="elComment_%d" class="ipsEntry js-ipsEntry ipsEntry--post">
<aside class="ipsEntry__author-column"><a href="https://%s/index.php?/profile/%d-member/" rel="nofollow"><img src="avatar.png" alt=""></a>
<h3 class="ipsEntry__username"><a href="https://%s/index.php?/profile/%d-member/">member_%d</a></h3></aside>
<div class="ipsEntry__header"><span class="ipsEntry__date"><time datetime="2026-04-%02dT08:30:00Z">April %d, 2026</time></span></div>
<div class="ipsRichText" data-role="commentContent"><p>Reply %d of the placeholder topic.</p></div>
</article>
`, 2000+number, forumInvisionHost, number, forumInvisionHost, number, number, number, number, number)
	}
	return `<!DOCTYPE html><html><head><title>Placeholder topic - Placeholder Community</title>
<meta property="og:title" content="Placeholder topic">
` + links + `</head>
<body class="ipsApp ipsApp_front" data-pageApp="forums" data-pageModule="forums" data-pageController="topic" data-id="77">
<ul class="ipsPagination" data-pages="` + fmt.Sprint(pages) + `" data-ipsPagination-perPage="2"></ul>
` + posts.String() + `</body></html>`
}

// forumSite serves topic pages by host and request URI, records every
// request, answers a listed one with its status, and, when answerTakes is
// set, moves its stepping clock on by that much per answer. directRefused:
// the direct client is answered 403 (unrecorded), only the Chrome rung's is
// served.
type forumSite struct {
	directRefused bool
	mu            sync.Mutex
	answers       map[string]string
	status        map[string]int
	requests      []string
	clock         *pacingClock
	answerTakes   time.Duration
}

func (site *forumSite) roundTrip(request *http.Request) (*http.Response, error) {
	key := request.URL.Host + request.URL.RequestURI()
	site.mu.Lock()
	site.requests = append(site.requests, key)
	site.mu.Unlock()
	if site.answerTakes > 0 {
		site.clock.advance(site.answerTakes)
	}
	if status := site.status[key]; status != 0 {
		return response(request, status, "text/html", "<html><body>You must be logged in.</body></html>"), nil
	}
	if body, ok := site.answers[key]; ok {
		return response(request, http.StatusOK, "text/html; charset=utf-8", body), nil
	}
	return response(request, http.StatusNotFound, "text/html", "<html><body>not found</body></html>"), nil
}

func (site *forumSite) harvester(t *testing.T) *Harvester {
	t.Helper()
	missing := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusNotFound, "application/json", `{}`), nil
	})
	direct := roundTripFunc(site.roundTrip)
	if site.directRefused {
		direct = func(request *http.Request) (*http.Response, error) {
			return response(request, http.StatusForbidden, "text/html", "<html><body>Access denied</body></html>"), nil
		}
	}
	return mustNew(t, Options{
		CacheDir:    t.TempDir(),
		Client:      &http.Client{Transport: direct},
		Chrome:      &http.Client{Transport: roundTripFunc(site.roundTrip)},
		Jina:        &http.Client{Transport: missing},
		OA:          &http.Client{Transport: missing},
		Converter:   &browserSpyConverter{},
		BrowserRung: browserOff(),
		Clock:       site.clock,
	})
}

// forumPostHeadings returns every "## " post heading in content, in order.
func forumPostHeadings(content string) []string {
	var headings []string
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, line)
		}
	}
	return headings
}

// TestForumTopicPagesAreFollowedAndJoinedInOrder: a phpBB or Invision topic
// is read page after page through its next link, its posts joined in page
// order, the pages read set against the pages the topic states; a later page
// refused, the fetch's page bound and its 180 s loader budget each stop the
// following, and the pages not read are named.
func TestForumTopicPagesAreFollowedAndJoinedInOrder(t *testing.T) {
	t.Parallel()
	phpbbHeadings := func(posts int) []string {
		var headings []string
		for number := 1; number <= posts; number++ {
			headings = append(headings, fmt.Sprintf("## member_%d · 2026-03-%02d 10:00 UTC · post %d",
				number, forumDay(number), 1000+number))
		}
		return headings
	}
	phpbbRequests := func(pages int) []string {
		var requests []string
		for page := 1; page <= pages; page++ {
			requests = append(requests, strings.TrimPrefix(forumPhpBBURL(page), "https://"))
		}
		return requests
	}
	cases := []struct {
		name    string
		engine  string
		pages   int
		refused int // a page answered 403, 0 for none
		foreign int // a page served as another topic's, 0 for none
		// edit, when set, rewrites each page's markup before it is served.
		edit        func(page int, markup string) string
		direct403   bool
		answerTakes time.Duration
		// url, when set, is the address the fetch begins at, served page 1.
		url      string
		header   []string
		headings []string
		requests []string
		partial  string
	}{
		{
			name:   "phpBB topic of three pages, every page read",
			engine: forumEnginePhpBB, pages: 3,
			header: []string{
				"# Placeholder topic",
				"**Topic:** " + forumPhpBBURL(1) + "  ",
				"**Pages:** 3 stated · 3 loaded (pages 1–3 read)  ",
				"**Posts:** 6 stated · 6 loaded",
			},
			headings: phpbbHeadings(6),
			requests: phpbbRequests(3),
		},
		{
			name:   "Invision topic whose third page is refused",
			engine: forumEngineInvision, pages: 3, refused: 3,
			header: []string{
				"# Placeholder topic",
				"**Topic:** " + forumInvisionURL(1) + "  ",
				"**Pages:** 3 stated · 2 loaded (pages 1–2 read) · gaps: page 3 not read (next page " +
					forumInvisionURL(3) + " not loaded)  ",
				"**Posts:** count not read (the page states no post count) · 4 loaded",
			},
			headings: []string{
				"## member_1 · 2026-04-01 08:30 UTC · post 2001",
				"## member_2 · 2026-04-02 08:30 UTC · post 2002",
				"## member_3 · 2026-04-03 08:30 UTC · post 2003",
				"## member_4 · 2026-04-04 08:30 UTC · post 2004",
			},
			requests: []string{
				strings.TrimPrefix(forumInvisionURL(1), "https://"),
				strings.TrimPrefix(forumInvisionURL(2), "https://"),
				strings.TrimPrefix(forumInvisionURL(3), "https://"),
			},
			partial: "invision topic: pages 1–2 of 3 read — page 3 not read (next page " + forumInvisionURL(3) +
				" not loaded); 1 loader request(s) failed (next page (page 3) (https://" + forumInvisionHost +
				"/index.php): HTTP 403)",
		},
		{
			name:   "Invision topic refusing the direct client, read through the Chrome rung",
			engine: forumEngineInvision, pages: 3, direct403: true,
			header: []string{
				"# Placeholder topic",
				"**Topic:** " + forumInvisionURL(1) + "  ",
				"**Pages:** 3 stated · 3 loaded (pages 1–3 read)  ",
				"**Posts:** count not read (the page states no post count) · 6 loaded",
			},
			headings: []string{
				"## member_1 · 2026-04-01 08:30 UTC · post 2001",
				"## member_2 · 2026-04-02 08:30 UTC · post 2002",
				"## member_3 · 2026-04-03 08:30 UTC · post 2003",
				"## member_4 · 2026-04-04 08:30 UTC · post 2004",
				"## member_5 · 2026-04-05 08:30 UTC · post 2005",
				"## member_6 · 2026-04-06 08:30 UTC · post 2006",
			},
			requests: []string{
				strings.TrimPrefix(forumInvisionURL(1), "https://"),
				strings.TrimPrefix(forumInvisionURL(2), "https://"),
				strings.TrimPrefix(forumInvisionURL(3), "https://"),
			},
		},
		{
			name:   "Invision topic whose second page answers as another topic",
			engine: forumEngineInvision, pages: 3, foreign: 2,
			header: []string{
				"# Placeholder topic",
				"**Topic:** " + forumInvisionURL(1) + "  ",
				"**Pages:** 3 stated · 1 loaded (page 1 read) · gaps: pages 2–3 not read (next page " +
					forumInvisionURL(2) + " not loaded)  ",
				"**Posts:** count not read (the page states no post count) · 2 loaded",
			},
			headings: []string{
				"## member_1 · 2026-04-01 08:30 UTC · post 2001",
				"## member_2 · 2026-04-02 08:30 UTC · post 2002",
			},
			requests: []string{
				strings.TrimPrefix(forumInvisionURL(1), "https://"),
				strings.TrimPrefix(forumInvisionURL(2), "https://"),
			},
			partial: "invision topic: page 1 of 3 read — pages 2–3 not read (next page " + forumInvisionURL(2) +
				" not loaded); 1 loader request(s) failed (next page (page 2) (https://" + forumInvisionHost +
				"/index.php): answered by a page (titled \"Placeholder topic - Placeholder Community\") " +
				"not verifiable as this topic)",
		},
		{
			name:   "phpBB topic past the page bound",
			engine: forumEnginePhpBB, pages: forumTopicPages + 5,
			header: []string{
				"# Placeholder topic",
				"**Topic:** " + forumPhpBBURL(1) + "  ",
				"**Pages:** 25 stated · 20 loaded (pages 1–20 read) · gaps: pages 21–25 not read: " +
					"the harvester reads at most 20 pages of a topic a fetch  ",
				"**Posts:** 50 stated · 40 loaded",
			},
			headings: phpbbHeadings(40),
			requests: phpbbRequests(20),
			partial: "phpbb topic: pages 1–20 of 25 read — pages 21–25 not read: " +
				"the harvester reads at most 20 pages of a topic a fetch",
		},
		{
			name:   "phpBB topic whose pages outlast the loader budget",
			engine: forumEnginePhpBB, pages: 6, answerTakes: 50 * time.Second,
			header: []string{
				"# Placeholder topic",
				"**Topic:** " + forumPhpBBURL(1) + "  ",
				"**Pages:** 6 stated · 4 loaded (pages 1–4 read) · gaps: pages 5–6 not read (next page " +
					forumPhpBBURL(5) + " not loaded)  ",
				"**Posts:** 12 stated · 8 loaded",
			},
			headings: phpbbHeadings(8),
			requests: phpbbRequests(4),
			partial: "phpbb topic: pages 1–4 of 6 read — pages 5–6 not read (next page " + forumPhpBBURL(5) +
				" not loaded); loader following stopped: the 180 s a fetch may spend following loaders ran out " +
				"after 3 request(s) and 153 s of it; the next wait 1 s and answer about 50 s would pass it; not requested",
		},
		{
			name:   "phpBB topic stating its post count with a dotted thousands separator",
			engine: forumEnginePhpBB, pages: 1,
			edit: func(_ int, markup string) string {
				return strings.ReplaceAll(markup, "\t2 posts\n", "\t1.234 posts\n")
			},
			header: []string{
				"# Placeholder topic",
				"**Topic:** " + forumPhpBBURL(1) + "  ",
				"**Pages:** 1 stated · 1 loaded (page 1 read) · gaps: 1232 stated post(s) in no page served  ",
				"**Posts:** 1234 stated · 2 loaded",
			},
			headings: phpbbHeadings(2),
			requests: phpbbRequests(1),
			partial:  "phpbb topic: page 1 of 1 read — 1232 stated post(s) in no page served",
		},
		{
			name:   "phpBB later page naming its topic only by the address it was asked at",
			engine: forumEnginePhpBB, pages: 3,
			edit: func(page int, markup string) string {
				if page != 2 {
					return markup
				}
				markup = strings.Replace(markup, `<link rel="canonical" href="https://forum.example/viewtopic.php?t=104&amp;start=2">`, "", 1)
				return strings.Replace(markup, `<a href="./viewtopic.php?t=104">Placeholder topic</a>`, "Placeholder topic", 1)
			},
			header: []string{
				"# Placeholder topic",
				"**Topic:** " + forumPhpBBURL(1) + "  ",
				"**Pages:** 3 stated · 1 loaded (page 1 read) · gaps: pages 2–3 not read (next page " +
					forumPhpBBURL(2) + " not loaded)  ",
				"**Posts:** 6 stated · 2 loaded",
			},
			headings: phpbbHeadings(2),
			requests: phpbbRequests(2),
			partial: "phpbb topic: page 1 of 3 read — pages 2–3 not read (next page " + forumPhpBBURL(2) +
				" not loaded); 1 loader request(s) failed (next page (page 2) (https://" + forumPhpBBHost +
				"/viewtopic.php): answered by a page (titled \"Placeholder topic - Placeholder Board\") " +
				"not verifiable as this topic)",
		},
		{
			name:   "phpBB topic whose page names no topic id",
			engine: forumEnginePhpBB, pages: 3,
			edit: func(page int, markup string) string {
				markup = strings.ReplaceAll(markup, `<link rel="canonical" href="https://forum.example/viewtopic.php?t=104">`, "")
				return strings.Replace(markup, `<a href="./viewtopic.php?t=104">Placeholder topic</a>`, "Placeholder topic", 1)
			},
			url: "https://" + forumPhpBBHost + "/viewtopic.php?p=1001",
			header: []string{
				"# Placeholder topic",
				"**Topic:** https://" + forumPhpBBHost + "/viewtopic.php?p=1001  ",
				"**Pages:** 3 stated · 1 loaded (page 1 read) · gaps: pages 2–3 not read: the page names no topic " +
					"id a later page could be proved this topic's by  ",
				"**Posts:** 6 stated · 2 loaded",
			},
			headings: phpbbHeadings(2),
			requests: []string{forumPhpBBHost + "/viewtopic.php?p=1001"},
			partial: "phpbb topic: page 1 of 3 read — pages 2–3 not read: the page names no topic id a later page " +
				"could be proved this topic's by",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pacing := newPacingClock()
			if tc.answerTakes > 0 {
				pacing = &pacingClock{Clock: clock.Real, stepping: true, at: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
			}
			site := &forumSite{answers: map[string]string{}, status: map[string]int{}, clock: pacing,
				answerTakes: tc.answerTakes, directRefused: tc.direct403}
			pageURL, pageHTML := forumPhpBBURL, forumPhpBBPage
			if tc.engine == forumEngineInvision {
				pageURL, pageHTML = forumInvisionURL, forumInvisionPage
			}
			for page := 1; page <= tc.pages; page++ {
				key := strings.TrimPrefix(pageURL(page), "https://")
				site.answers[key] = pageHTML(page, tc.pages)
				if tc.edit != nil {
					site.answers[key] = tc.edit(page, site.answers[key])
				}
				if page == tc.foreign {
					site.answers[key] = strings.Replace(site.answers[key], `data-id="77"`, `data-id="78"`, 1)
				}
				if page == tc.refused {
					site.status[key] = http.StatusForbidden
				}
			}
			start := pageURL(1)
			if tc.url != "" {
				start = tc.url
				site.answers[strings.TrimPrefix(tc.url, "https://")] = site.answers[strings.TrimPrefix(pageURL(1), "https://")]
			}
			result := site.harvester(t).FetchWithOptions(context.Background(), start, FetchOptions{Refresh: true})
			if result.Error != "" {
				t.Fatalf("error %q", result.Error)
			}
			header := strings.Split(strings.SplitN(result.Content, "\n\n---\n\n", 2)[0], "\n")
			header = append(header[:1], header[2:]...) // the blank line under the title
			if strings.Join(header, "\n") != strings.Join(tc.header, "\n") {
				t.Fatalf("header:\n got %q\nwant %q\n%s", header, tc.header, result.Content)
			}
			if got := forumPostHeadings(result.Content); strings.Join(got, "\n") != strings.Join(tc.headings, "\n") {
				t.Fatalf("posts:\n got %q\nwant %q", got, tc.headings)
			}
			if strings.Join(site.requests, "\n") != strings.Join(tc.requests, "\n") {
				t.Fatalf("requests:\n got %q\nwant %q", site.requests, tc.requests)
			}
			if result.Partial != tc.partial {
				t.Fatalf("partial:\n got %q\nwant %q", result.Partial, tc.partial)
			}
		})
	}
}
