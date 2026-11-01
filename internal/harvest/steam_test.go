package harvest

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The fixtures are a captured Steam store page and two pages of its review
// API answer (testdata/steam), trimmed and scrubbed: the app an invented one,
// the page cut to its name, review summary, release date, developer and
// description markup; the reviews cut to five across two cursor pages, the
// reviewers' ids, persona names, profile addresses and avatars placeholders,
// the texts placeholder text keeping the BBCode shape the API serves; the
// first page's query_summary states the five as the app's total, the second
// page's states only its own num_reviews, as the API does.
const (
	steamURL      = "https://store.steampowered.com/app/4040/An_Example_Game/"
	steamPagePath = "store.steampowered.com/app/4040/An_Example_Game/"
	steamReviews  = "store.steampowered.com/appreviews/4040?cursor=%s&filter=recent&json=1&language=all" +
		"&num_per_page=100&purchase_type=all"
)

func steamSite(t *testing.T) *socialSite {
	return &socialSite{apiPath: "/appreviews/", answers: map[string]string{
		steamPagePath:                               socialFixture(t, "steam/app-page.html"),
		fmt.Sprintf(steamReviews, "%2A"):            socialFixture(t, "steam/reviews-1.json"),
		fmt.Sprintf(steamReviews, "AoJwPAGEONE%3D"): socialFixture(t, "steam/reviews-2.json"),
	}}
}

// TestSteamAppLoadsItsReviews: a Steam store page renders its description,
// and its reviews are read from the store's review API on the page's own host,
// page after page by the cursor each answer names, sending no credential;
// every review renders once in the API's order with its verdict and its
// BBCode as Markdown; the stated total is the API's own; the artifact is
// complete, and a second harvest is identical.
func TestSteamAppLoadsItsReviews(t *testing.T) {
	t.Parallel()
	site := steamSite(t)
	h := site.harvester(t)
	result := h.FetchWithOptions(context.Background(), steamURL, FetchOptions{Refresh: true})
	if result.Error != "" || result.Partial != "" {
		t.Fatalf("the app is not complete: partial=%q error=%q\n%.2500s", result.Partial, result.Error,
			result.Content)
	}
	want := "100000001,100000002,100000003,100000004,100000005"
	if got := strings.Join(socialRendered(result.Content), ","); got != want {
		t.Fatalf("reviews or their order are wrong: got %v, want %v\n%.2500s", got, want, result.Content)
	}
	for _, text := range []string{
		"# An Example Game",
		"**Author:** Example Studio · **Posted:** 18 Apr, 2011",
		"**Post:** " + steamURL,
		"**Replies:** 5 stated · 5 loaded",
		"## About This Game",
		"An Example Game draws from a **placeholder** formula of puzzles and story.",
		"- A first feature",
		"**Recommended**",
		"**Not recommended**",
		"**A bold opening** of review 1.",
		"- a first point",
		"A closing *line*.",
		"(https://steamcommunity.com/profiles/76561198000000001/recommended/4040/)",
	} {
		if !strings.Contains(result.Content, text) {
			t.Fatalf("the artifact lacks %q:\n%.2500s", text, result.Content)
		}
	}
	if len(site.requests) != 2 {
		t.Fatalf("review API requests %v, want the two cursor pages", site.requests)
	}
	for _, header := range site.headers {
		if header.Get("Authorization") != "" || header.Get("Cookie") != "" {
			t.Fatalf("an API request carried a credential: %v", header)
		}
	}
	again := h.FetchWithOptions(context.Background(), steamURL, FetchOptions{Refresh: true})
	if again.Content != result.Content {
		t.Fatal("a second harvest of the same app differs")
	}
}

// TestSteamReviewWithNoIdIsKept: a review the API serves with no
// recommendationid is not a reason to refuse its page: it renders under an
// unknown id, and the other reviews render as before.
func TestSteamReviewWithNoIdIsKept(t *testing.T) {
	t.Parallel()
	site := steamSite(t)
	first := fmt.Sprintf(steamReviews, "%2A")
	site.answers[first] = strings.Replace(site.answers[first], `"recommendationid": "100000002",`, ``, 1)
	result := site.harvester(t).FetchWithOptions(context.Background(), steamURL, FetchOptions{Refresh: true})
	want := "100000001,unknown id,100000003,100000004,100000005"
	if got := strings.Join(socialRendered(result.Content), ","); got != want || result.Partial != "" {
		t.Fatalf("reviews are wrong: got %v, want %v, partial=%q\n%.2500s", got, want, result.Partial, result.Content)
	}
}

// TestSteamReviewsBeyondTheBoundAreNamed: an app stating more reviews than
// the bounded page count reads renders the most recent pages and names the
// rest, stated · loaded, in the partial marker.
func TestSteamReviewsBeyondTheBoundAreNamed(t *testing.T) {
	t.Parallel()
	site := steamSite(t)
	site.answers[fmt.Sprintf(steamReviews, "%2A")] = strings.Replace(site.answers[fmt.Sprintf(steamReviews, "%2A")],
		`"total_reviews": 5`, `"total_reviews": 467071`, 1)
	for page := 2; page < steamReviewPages; page++ {
		answer := strings.ReplaceAll(site.answers[fmt.Sprintf(steamReviews, "AoJwPAGEONE%3D")], "AoJwPAGETWO=",
			fmt.Sprintf("AoJwPAGE%d=", page+1))
		answer = strings.ReplaceAll(answer, `"10000000`, fmt.Sprintf(`"1%d000000`, page))
		cursor := "AoJwPAGETWO%3D"
		if page > 2 {
			cursor = fmt.Sprintf("AoJwPAGE%d%%3D", page)
		}
		site.answers[fmt.Sprintf(steamReviews, cursor)] = answer
	}
	result := site.harvester(t).FetchWithOptions(context.Background(), steamURL, FetchOptions{Refresh: true})
	loaded := 3 + 2*(steamReviewPages-1)
	if result.Error != "" || len(socialRendered(result.Content)) != loaded {
		t.Fatalf("the loaded reviews were not rendered: error=%q\n%.2500s", result.Error, result.Content)
	}
	if len(site.requests) != steamReviewPages {
		t.Fatalf("review API requests %v, want %d", site.requests, steamReviewPages)
	}
	for _, want := range []string{
		fmt.Sprintf("steam app: %d of 467071 stated replies loaded", loaded),
		fmt.Sprintf("%d stated repl(ies) not served: the harvester reads the most recent", 467071-loaded),
	} {
		if !strings.Contains(result.Partial, want) {
			t.Fatalf("the partial marker lacks %q: %q", want, result.Partial)
		}
	}
}

// TestSteamReviewsNotLoadedNameTheGap: a review answer the API refuses, or one
// that is not its review list, is never rendered: the app renders with the
// count its page states named unread, never as an app with no reviews; a page
// whose count is not a number, or states none, renders its count unstated,
// never a made-up zero.
func TestSteamReviewsNotLoadedNameTheGap(t *testing.T) {
	t.Parallel()
	first := fmt.Sprintf(steamReviews, "%2A")
	refuse := func(site *socialSite) { site.status = map[string]int{first: http.StatusForbidden} }
	const unread = "steam app: 0 of 4 stated replies loaded — 4 stated repl(ies) not read; "
	const unstated = "steam app: 0 replies loaded, none stated — replies not read; "
	const refused = "1 loader request(s) failed (the app's review page 1 (https://store.steampowered.com/appreviews/" +
		"4040): HTTP 403)"
	for _, tc := range []struct {
		name   string
		mutate func(site *socialSite)
		want   string
	}{
		{"refused", refuse, unread + refused},
		{"not success", func(site *socialSite) { site.answers[first] = `{"success":2}` }, unread +
			"1 loader request(s) failed (the app's review page 1 (https://store.steampowered.com/appreviews/4040): " +
			"answered by a application/json body that is not a page of the store's review list)"},
		{"count not a number", func(site *socialSite) {
			refuse(site)
			site.answers[steamPagePath] = strings.Replace(site.answers[steamPagePath],
				`<meta itemprop="reviewCount" content="4">`, `<meta itemprop="reviewCount" content="4,000">`, 1)
		}, unstated + refused},
		{"count absent", func(site *socialSite) {
			refuse(site)
			site.answers[steamPagePath] = strings.Replace(site.answers[steamPagePath],
				`<meta itemprop="reviewCount" content="4">`, ``, 1)
		}, unstated + refused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := steamSite(t)
			tc.mutate(site)
			result := site.servingHarvester(t).FetchWithOptions(context.Background(), steamURL,
				FetchOptions{Refresh: true})
			if result.Partial != tc.want {
				t.Fatalf("the partial marker does not name the unread reviews: got %q, want %q (error %q)\n%.1500s",
					result.Partial, tc.want, result.Error, result.Content)
			}
			if got := socialRendered(result.Content); len(got) != 0 {
				t.Fatalf("reviews rendered from an answer not proved the app's: %v", got)
			}
		})
	}
}
