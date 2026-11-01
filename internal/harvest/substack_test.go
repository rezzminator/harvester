package harvest

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// The fixtures are a captured Substack post page and its comments answer
// (testdata/substack), trimmed and scrubbed: the publication moved to an
// invented custom domain (so only the markup can name it Substack), names and
// handles placeholders, bodies placeholder text, the page cut to its
// substackcdn.com assets and its window._preloads script holding the captured
// post keys, and the comment tree cut to seven published comments plus the
// captured unavailable shapes: a deleted comment (body null, deleted true), a
// blocked one and one a moderator removed, each with its status; the post's
// comment_count is the published comments alone, as the API states it.
const (
	substackURL         = "https://www.example-pub.com/p/a-post"
	substackCommentsAPI = "www.example-pub.com/api/v1/post/214878061/comments?all_comments=true&sort=best_first"
)

func substackSite(t *testing.T) *socialSite {
	return &socialSite{answers: map[string]string{
		"www.example-pub.com/p/a-post": socialFixture(t, "substack/post-page.html"),
		substackCommentsAPI:            socialFixture(t, "substack/comments.json"),
	}}
}

var substackWant = []string{
	"337312099", "337314335", "337321477", "337313520", "337316944", "337318085", "337319266",
}

// TestSubstackPostLoadsEveryComment: a Substack post on a custom domain is
// known by its markup; its comments are read from the publication's API on the
// page's own host, sending no credential; every comment renders once in the
// API's tree order, a reply nested under its parent, the deleted, blocked and
// removed comments named apart from the stated count; the artifact is
// complete, and a second harvest is identical.
func TestSubstackPostLoadsEveryComment(t *testing.T) {
	t.Parallel()
	site := substackSite(t)
	h := site.harvester(t)
	result := h.FetchWithOptions(context.Background(), substackURL, FetchOptions{Refresh: true})
	if result.Error != "" || result.Partial != "" {
		t.Fatalf("the post is not complete: partial=%q error=%q\n%.1500s", result.Partial, result.Error,
			result.Content)
	}
	if got := socialRendered(result.Content); strings.Join(got, ",") != strings.Join(substackWant, ",") {
		t.Fatalf("comments or their order are wrong: got %v, want %v\n%.2500s", got, substackWant, result.Content)
	}
	for _, text := range []string{
		"# A post title",
		"**Author:** @author-0",
		"**Post:** " + substackURL,
		"**Replies:** 7 stated · 7 loaded · 1 deleted (not in the stated count) · 1 blocked (not in the stated " +
			"count) · 1 removed by a moderator (not in the stated count)",
		"The post body, with **bold** and a [link](https://example.com/).",
		"[337314335](" + substackURL + "/comment/337314335)",
		"\n      - **@user-2**", // a reply under a deleted reply, nested
		"*a reply deleted*",
		"  > A quoted line of the post.",
	} {
		if !strings.Contains(result.Content, text) {
			t.Fatalf("the artifact lacks %q:\n%.2500s", text, result.Content)
		}
	}
	if strings.Join(site.requests, "\n") != substackCommentsAPI {
		t.Fatalf("API requests %v", site.requests)
	}
	for _, header := range site.headers {
		if header.Get("Authorization") != "" || header.Get("Cookie") != "" {
			t.Fatalf("an API request carried a credential: %v", header)
		}
	}
	again := h.FetchWithOptions(context.Background(), substackURL, FetchOptions{Refresh: true})
	if again.Content != result.Content {
		t.Fatal("a second harvest of the same post differs")
	}
}

// TestSubstackUnservedCommentsFlagThePartial: a post stating more comments
// than its tree serves renders what was served and names the rest.
func TestSubstackUnservedCommentsFlagThePartial(t *testing.T) {
	t.Parallel()
	site := substackSite(t)
	site.answers["www.example-pub.com/p/a-post"] = strings.Replace(site.answers["www.example-pub.com/p/a-post"],
		`\"comment_count\":7`, `\"comment_count\":400`, 1)
	result := site.harvester(t).FetchWithOptions(context.Background(), substackURL, FetchOptions{Refresh: true})
	if result.Error != "" || len(socialRendered(result.Content)) != 7 {
		t.Fatalf("the served comments were not rendered: error=%q\n%.1500s", result.Error, result.Content)
	}
	for _, want := range []string{"substack post: 7 of 400 stated replies loaded", "393 stated repl(ies) not served"} {
		if !strings.Contains(result.Partial, want) {
			t.Fatalf("the partial marker lacks %q: %q", want, result.Partial)
		}
	}
}

// TestSubstackCommentsNotLoadedNameTheGap: a comment tree the API refuses, one
// of another post, or one in which no comment names the post, is never rendered: the post renders with its stated
// comments named unread, never as a post with no comments.
func TestSubstackCommentsNotLoadedNameTheGap(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(site *socialSite){
		"refused": func(site *socialSite) { site.status = map[string]int{substackCommentsAPI: http.StatusNotFound} },
		"another post's": func(site *socialSite) {
			site.answers[substackCommentsAPI] = strings.ReplaceAll(site.answers[substackCommentsAPI],
				`"post_id":214878061`, `"post_id":1`)
		},
		"naming no post": func(site *socialSite) {
			site.answers[substackCommentsAPI] = strings.ReplaceAll(site.answers[substackCommentsAPI],
				`"post_id":214878061`, `"post":214878061`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			site := substackSite(t)
			mutate(site)
			result := site.servingHarvester(t).FetchWithOptions(context.Background(), substackURL,
				FetchOptions{Refresh: true})
			if !strings.Contains(result.Partial, "7 stated repl(ies) not read") {
				t.Fatalf("the partial marker does not name the unread comments: %q (error %q)\n%.1500s",
					result.Partial, result.Error, result.Content)
			}
			if got := socialRendered(result.Content); len(got) != 0 {
				t.Fatalf("comments rendered from an answer not proved this post's: %v", got)
			}
		})
	}
}

// TestSubstackStatesNoCountAndServesACommentWithoutAnID: a post preloading no
// comment_count still renders, its count line reading unstated, and a comment
// the API serves with no id stays in the tree under a key of its own,
// linked to the post alone; neither makes the artifact partial.
func TestSubstackStatesNoCountAndServesACommentWithoutAnID(t *testing.T) {
	t.Parallel()
	site := substackSite(t)
	site.answers["www.example-pub.com/p/a-post"] = strings.Replace(site.answers["www.example-pub.com/p/a-post"],
		`\"comment_count\":7`, `\"comment_counts\":7`, 1)
	site.answers[substackCommentsAPI] = strings.Replace(site.answers[substackCommentsAPI], `{"id":337313520,`, `{`, 1)
	result := site.harvester(t).FetchWithOptions(context.Background(), substackURL, FetchOptions{Refresh: true})
	if result.Error != "" || result.Partial != "" {
		t.Fatalf("the post is not complete: partial=%q error=%q\n%.1500s", result.Partial, result.Error,
			result.Content)
	}
	want := strings.Replace(strings.Join(substackWant, ","), "337313520", "no-id-4", 1)
	if got := strings.Join(socialRendered(result.Content), ","); got != want {
		t.Fatalf("comments or their order are wrong: got %v, want %v\n%.2500s", got, want, result.Content)
	}
	for _, text := range []string{
		"**Replies:** unstated · 7 loaded · 1 deleted · 1 blocked · 1 removed by a moderator\n\n",
		"[no-id-4](" + substackURL + ")",
	} {
		if !strings.Contains(result.Content, text) {
			t.Fatalf("the artifact lacks %q:\n%.2500s", text, result.Content)
		}
	}
}

// TestKeptAnswerThatNoLongerDecodesIsNamed: a kept API answer a later reading
// cannot decode is left out, and the extraction names it where it names its
// other gaps — the partial marker, or the reason an X post is not rendered —
// beside what the loss leaves unread.
func TestKeptAnswerThatNoLongerDecodesIsNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		page    string
		address string
		tag     string
		kind    string
		kept    string
		extract func(*html.Node, *url.URL) (siteExtraction, bool)
		want    string
	}{
		{
			"substack", "substack/post-page.html", substackURL, substackAnswerTag, "comments", `{"comments":7}`,
			extractSubstackPost,
			"substack post: 0 of 7 stated replies loaded — 1 kept Substack answer(s) could not be replayed and " +
				"were left out; 7 stated repl(ies) not read",
		},
		{
			"slashdot", "slashdot/story-page.html", slashdotURL, slashdotAnswerTag, "", `{`, extractSlashdotStory,
			"slashdot story: 3 of 6 stated replies loaded — 1 kept Slashdot answer(s) could not be replayed " +
				"and were left out; 3 stated repl(ies) not served: the discussion's further comments were not requested",
		},
		{
			"product hunt", "producthunt/reviews-page.html", productHuntURL, productHuntAnswerTag, "reviews",
			`{"data":7}`, extractProductHuntReviews,
			"product hunt reviews: 2 of 5 stated replies loaded — 1 kept Product Hunt answer(s) could not be " +
				"replayed and were left out; 3 stated repl(ies) not served: the page embeds only its first " +
				"reviews; the rest are in the site's review list, which was not read",
		},
		{
			"devto", "devto/article-page.html", devtoURL, devtoAnswerTag, devtoKindComments, `{`, extractDevtoArticle,
			"dev.to article: the article's API record was not loaded (its comments not read from the API; the " +
				"page is stored as dev.to served it); 1 kept dev.to answer(s) could not be replayed and were left out",
		},
		{
			"lemmy", "lemmy/post-page.html", "https://lemmy.example" + lemmyPath, lemmyAnswerTag, lemmyKindComments,
			`{`, extractLemmyPost,
			"lemmy post: the post's API record was not loaded (its comments not read from the API; the page is " +
				"stored as the instance served it); 1 kept Lemmy answer(s) could not be replayed and were left out",
		},
		{
			"mastodon", "mastodon/status-page.html", mastoURL, mastodonAnswerTag, mastodonKindStatus, `{`,
			extractMastodonStatus,
			"mastodon status: the status's API record was not loaded (its replies not read from the API; the " +
				"page is stored as the instance served it); 1 kept Mastodon answer(s) could not be replayed and " +
				"were left out",
		},
		{
			"stack exchange", "stackexchange/challenge.html", seQuestionURL, seAnswerTag, seKindQuestion, `{`,
			extractStackExchangeQuestion,
			"stackexchange question: the question's API record was not loaded (its answers, comments and stated " +
				"counts not read from the API; the page is stored as rendered); 1 kept Stack Exchange answer(s) " +
				"could not be replayed and were left out",
		},
		{
			"x", "x/post-page.html", xTestURL, xAnswerTag, xKindThread, `{"status":7}`, extractXPost,
			"x post: the post was not loaded from the FxTwitter API (the page is stored as X serves it " +
				"signed out); 1 kept X answer(s) could not be replayed and were left out",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc, err := html.Parse(strings.NewReader(socialFixture(t, tc.page)))
			if err != nil {
				t.Fatal(err)
			}
			page, err := url.Parse(tc.address)
			if err != nil {
				t.Fatal(err)
			}
			keepAnswer(doc, tc.tag, "kept", tc.kind, 0, []byte(tc.kept))
			extraction, ok := tc.extract(doc, page)
			if got := extraction.partial + extraction.unrendered; got != tc.want {
				t.Fatalf("the left-out answer is not named (claimed %v): got %q, want %q", ok, got, tc.want)
			}
		})
	}
}

// TestXPostNamesAKeptAnswerThatNoLongerDecodes: beside a kept answer that does
// decode, one that does not is named in the partial marker of the post.
func TestXPostNamesAKeptAnswerThatNoLongerDecodes(t *testing.T) {
	t.Parallel()
	doc, err := html.Parse(strings.NewReader(socialFixture(t, "x/post-page.html")))
	if err != nil {
		t.Fatal(err)
	}
	page, err := url.Parse(xTestURL)
	if err != nil {
		t.Fatal(err)
	}
	keepAnswer(doc, xAnswerTag, "bad", xKindThread, 0, []byte(`{"status":7}`))
	keepAnswer(doc, xAnswerTag, "good", xKindThread, 0, []byte(xStatusAnswer(t, nil)))
	extraction, ok := extractXPost(doc, page)
	if want := "x post: 1 kept X answer(s) could not be replayed and were left out"; !ok || extraction.partial != want {
		t.Fatalf("partial %q (claimed %v), want %q", extraction.partial, ok, want)
	}
}

// TestKeptAnswerThatNoLongerDecodesIsNamedBesideTheRenderedRecord: where the
// record of a thread is kept and a later answer is not, the thread renders
// from the record and the left-out answer is named in its partial marker.
func TestKeptAnswerThatNoLongerDecodesIsNamedBesideTheRenderedRecord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		page       string
		address    string
		tag        string
		record     string
		recordKind string
		badKind    string
		extract    func(*html.Node, *url.URL) (siteExtraction, bool)
		want       string
	}{
		{
			"devto", "devto/article-page.html", devtoURL, devtoAnswerTag, "devto/article.json", devtoKindArticle,
			devtoKindComments, extractDevtoArticle,
			"dev.to article: 0 of 12 stated replies loaded — 1 kept dev.to answer(s) could not be replayed and were left out; 12 stated repl(ies) not read",
		},
		{
			"lemmy", "lemmy/post-page.html", "https://lemmy.example" + lemmyPath, lemmyAnswerTag, "lemmy/post.json",
			lemmyKindPost, lemmyKindComments, extractLemmyPost,
			"lemmy post: 0 of 55 stated replies loaded — 1 kept Lemmy answer(s) could not be replayed and were left out; 55 stated repl(ies) not read",
		},
		{
			"mastodon", "mastodon/status-page.html", mastoURL, mastodonAnswerTag, "mastodon/status.json",
			mastodonKindStatus, mastodonKindCtx, extractMastodonStatus,
			"mastodon status: 0 of 7 stated replies loaded — 1 kept Mastodon answer(s) could not be replayed and were left out; the status's context API answer was not loaded; 7 stated repl(ies) not read",
		},
		{
			"stack exchange", "stackexchange/challenge.html", seQuestionURL, seAnswerTag, "stackexchange/question.json",
			seKindQuestion, seKindAnswers, extractStackExchangeQuestion,
			"stackexchange question: 0 of 105 answers, 13 of 13 stated comments loaded — answers page 1 not loaded (answers from #1 on, and their comments, not read); 1 kept Stack Exchange answer(s) could not be replayed and were left out",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc, err := html.Parse(strings.NewReader(socialFixture(t, tc.page)))
			if err != nil {
				t.Fatal(err)
			}
			page, err := url.Parse(tc.address)
			if err != nil {
				t.Fatal(err)
			}
			keepAnswer(doc, tc.tag, "record", tc.recordKind, 0, []byte(socialFixture(t, tc.record)))
			keepAnswer(doc, tc.tag, "bad", tc.badKind, 1, []byte(`{`))
			extraction, ok := tc.extract(doc, page)
			if !ok || extraction.partial != tc.want {
				t.Fatalf("the left-out answer is not named beside the record (rendered %v): got %q, want %q",
					ok, extraction.partial, tc.want)
			}
		})
	}
}

// TestPagedAnswerThatNoLongerDecodesCostsThatPageAlone: of the kept review
// pages the first no longer decodes; the page after it still renders, the loss
// is named in the partial marker, and the next page a loader asks for follows
// the one left out (it is never asked for again).
func TestPagedAnswerThatNoLongerDecodesCostsThatPageAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		page      string
		address   string
		tag       string
		kind      string
		second    string
		extract   func(*html.Node, *url.URL) (siteExtraction, bool)
		next      func(*html.Node, *url.URL) int
		wantFirst string
		want      string
	}{
		{
			"imdb", "stackexchange/challenge.html", imdbURL, imdbAnswerTag, "reviews", "imdb/reviews-2.json",
			extractIMDbReviews,
			func(doc *html.Node, page *url.URL) int { state, _ := imdbPageOf(doc, page); return state.next },
			"Placeholder text of the fourth review.",
			"imdb reviews: 2 of 4 stated replies loaded — 1 kept IMDb answer(s) could not be replayed and were " +
				"left out; 2 stated repl(ies) not served: IMDb's GraphQL API ended its review list (no further " +
				"page) before its stated total",
		},
		{
			"steam", "steam/app-page.html", steamURL, steamAnswerTag, "reviews", "steam/reviews-2.json",
			extractSteamApp,
			func(doc *html.Node, page *url.URL) int { state, _ := steamPageOf(doc, page); return state.next },
			"Placeholder review text 5.",
			// The page stating the total is the one left out: the count is unstated, never the page's own.
			"steam app: 2 replies loaded, none stated — 1 kept Steam answer(s) could not be replayed and were " +
				"left out",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc, err := html.Parse(strings.NewReader(socialFixture(t, tc.page)))
			if err != nil {
				t.Fatal(err)
			}
			page, err := url.Parse(tc.address)
			if err != nil {
				t.Fatal(err)
			}
			keepAnswer(doc, tc.tag, "page 0", tc.kind, 0, []byte(`{`))
			keepAnswer(doc, tc.tag, "page 1", tc.kind, 1, []byte(socialFixture(t, tc.second)))
			extraction, ok := tc.extract(doc, page)
			if !ok || extraction.partial != tc.want {
				t.Fatalf("the left-out page is not named beside the kept one (rendered %v): got %q, want %q",
					ok, extraction.partial, tc.want)
			}
			if got := tc.next(doc, page); got != 2 {
				t.Fatalf("the next page asked for is %d, want 2 (past the page left out)", got)
			}
			if got := extraction.markdown; !strings.Contains(got, tc.wantFirst) {
				t.Fatalf("the page after the left-out one is not rendered: want %q in\n%.2500s", tc.wantFirst, got)
			}
		})
	}
}
