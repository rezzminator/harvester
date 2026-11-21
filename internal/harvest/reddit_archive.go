package harvest

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rezzminator/harvester/internal/obs"
)

// The Reddit archive rung. Reddit refuses many server addresses outright — the
// page, old.reddit.com, the JSON and RSS twins and the readers that fetch on
// our behalf all answer HTTP 403 — so a Reddit address no live rung could read
// is answered from the Arctic Shift public archive, which keeps Reddit's posts
// and comments as data. The answer is the archive's record of the day it last
// retrieved them, never the live page: it is stored partial under a reason
// that says so (redditArchiveReason), and scores and comment counts are the
// archive's. Four shapes are answered — a thread (or one comment's branch), a
// subreddit's listing, a search inside one subreddit, a subreddit's about
// page; any other Reddit address is left to the next rung.

const (
	rungRedditArchive = "reddit-archive"
	redditArchiveBase = "https://arctic-shift.photon-reddit.com"
	// redditArchivePage is the archive's largest page of posts, and
	// redditArchivePages the pages a ranking or a local search reads: the
	// archive orders posts by date only, so "top" and a search it cannot run
	// are computed here over the newest redditArchivePage*redditArchivePages.
	redditArchivePage  = 100
	redditArchivePages = 10
	// redditArchiveTree is the archive's largest comment tree in one answer.
	redditArchiveTree     = 25000
	redditArchiveMaxBytes = 64 << 20
	// redditArchiveExcerpt is how much of a post's own text a listing shows.
	redditArchiveExcerpt = 400

	redditShapeThread  = "thread"
	redditShapeListing = "listing"
	redditShapeSearch  = "search"
	redditShapeAbout   = "about"

	redditSortTop = "top"
	redditSortNew = "new"
)

// redditArchiveAsk is what a Reddit address asks for, in the archive's terms.
type redditArchiveAsk struct {
	shape     string
	subreddit string
	post      string // thread: the post's id
	comment   string // thread: the comment a permalink names, its branch alone
	order     string // listing or search: the sort the address names
	window    string // listing or search: Reddit's t= value
	query     string // search: the words
}

// The archive's two failures, as a refused Reddit address's failure names
// them: the ladder's own message quotes the archive's error after
// redditArchiveUnread, the published one (PublicFailureMessage) never does.
const (
	redditArchiveNoRecord = " The archive of Reddit that stands in for a refused page holds no record of this address."
	redditArchiveUnread   = " The archive of Reddit that stands in for a refused page could not be read either"
	redditArchiveRetry    = "; a retry later may reach it."
)

// redditArchiveWaits are the pauses before each repeat of a call the archive
// shed: under load it answers HTTP 422 ("Timeout. Maybe slow down a bit"), a
// 429 or a server error to a call it serves seconds later.
var redditArchiveWaits = []time.Duration{2 * time.Second, 5 * time.Second}

// redditArchiveShed reports an answer the archive gives a call it may serve
// when asked again.
func redditArchiveShed(err error) bool {
	var status httpStatusError
	if !errors.As(err, &status) {
		return false
	}
	return status == http.StatusUnprocessableEntity || status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError
}

var (
	redditSubredditName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	redditThingID       = regexp.MustCompile(`^[a-z0-9]+$`)
)

// redditListingOrders are the listing pages of a subreddit.
var redditListingOrders = map[string]bool{
	"hot": true, redditSortNew: true, redditSortTop: true, "rising": true, "controversial": true, "best": true,
}

// redditWindows maps Reddit's t= values to the span they cover; "all" is none.
var redditWindows = map[string]time.Duration{
	"hour":  time.Hour,
	"day":   24 * time.Hour,
	"week":  7 * 24 * time.Hour,
	"month": 30 * 24 * time.Hour,
	"year":  365 * 24 * time.Hour,
}

// redditArchiveAskFor reads a Reddit address as an ask the archive can answer;
// false for any other address and for a Reddit page with no archive shape (a
// profile, a wiki, the front page, a search across all of Reddit).
func redditArchiveAskFor(source string) (redditArchiveAsk, bool) {
	page, err := url.Parse(source)
	if err != nil {
		return redditArchiveAsk{}, false
	}
	host := strings.ToLower(page.Hostname())
	if host != redditHost && !strings.HasSuffix(host, "."+redditHost) {
		return redditArchiveAsk{}, false
	}
	// The JSON and RSS twins of a page (".../about.json", ".../.rss") ask for
	// the page's own content.
	path := strings.Trim(page.Path, "/")
	path = strings.TrimSuffix(strings.TrimSuffix(path, ".json"), ".rss")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	query := page.Query()
	ask := redditArchiveAsk{order: strings.ToLower(query.Get("sort")), window: strings.ToLower(query.Get("t"))}
	thread := func(rest []string) (redditArchiveAsk, bool) {
		// rest is "comments", the post id, then a slug and a comment id.
		if len(rest) < 2 || !redditThingID.MatchString(rest[1]) {
			return redditArchiveAsk{}, false
		}
		ask.shape, ask.post = redditShapeThread, rest[1]
		if len(rest) >= 4 && redditThingID.MatchString(rest[3]) {
			ask.comment = rest[3]
		}
		return ask, true
	}
	switch {
	case parts[0] == "comments":
		return thread(parts)
	case (parts[0] == "user" || parts[0] == "u") && len(parts) >= 4 && parts[2] == "comments":
		return thread(parts[2:])
	case parts[0] != "r" || len(parts) < 2 || !redditSubredditName.MatchString(parts[1]):
		return redditArchiveAsk{}, false
	}
	ask.subreddit = parts[1]
	rest := parts[2:]
	switch {
	case len(rest) == 0:
		ask.shape = redditShapeListing
	case rest[0] == "comments":
		return thread(rest)
	case len(rest) > 1:
		return redditArchiveAsk{}, false
	case rest[0] == "about":
		ask.shape = redditShapeAbout
	case rest[0] == "search":
		ask.shape, ask.query = redditShapeSearch, strings.TrimSpace(query.Get("q"))
		if ask.query == "" {
			return redditArchiveAsk{}, false
		}
	case redditListingOrders[rest[0]]:
		ask.shape, ask.order = redditShapeListing, rest[0]
	default:
		return redditArchiveAsk{}, false
	}
	return ask, true
}

// redditArchiveEpoch is a Unix time the archive sends as a number, or as a
// quoted number in records imported from older dumps.
type redditArchiveEpoch int64

func (epoch *redditArchiveEpoch) UnmarshalJSON(raw []byte) error {
	text := strings.Trim(string(bytes.TrimSpace(raw)), `"`)
	if text == "" || text == "null" {
		return nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return fmt.Errorf("reddit archive time %s: %w", raw, err)
	}
	*epoch = redditArchiveEpoch(value)
	return nil
}

// day is the UTC calendar date, empty for a time the record does not carry.
func (epoch redditArchiveEpoch) day() string {
	if epoch <= 0 {
		return ""
	}
	return time.Unix(int64(epoch), 0).UTC().Format(time.DateOnly)
}

// redditArchivePost is one post as the archive records it.
type redditArchivePost struct {
	ID          string             `json:"id"`
	Title       string             `json:"title"`
	Author      string             `json:"author"`
	Subreddit   string             `json:"subreddit"`
	Selftext    string             `json:"selftext"`
	URL         string             `json:"url"`
	Permalink   string             `json:"permalink"`
	Score       int                `json:"score"`
	NumComments int                `json:"num_comments"`
	Created     redditArchiveEpoch `json:"created_utc"`
	IsSelf      bool               `json:"is_self"`
	Retrieved   redditArchiveEpoch `json:"retrieved_on"`
	Meta        struct {
		RetrievedAgain redditArchiveEpoch `json:"retrieved_2nd_on"`
	} `json:"_meta"`
}

// thread is the post's address on Reddit.
func (post redditArchivePost) thread() string {
	permalink := post.Permalink
	if permalink == "" {
		permalink = "/comments/" + post.ID + "/"
		if post.Subreddit != "" {
			permalink = "/r/" + post.Subreddit + permalink
		}
	}
	return redditBase().ResolveReference(&url.URL{Path: permalink}).String()
}

// retrieved is the day the archive last read the post.
func (post redditArchivePost) retrieved() redditArchiveEpoch {
	return max(post.Retrieved, post.Meta.RetrievedAgain)
}

// redditArchiveNode is one node of the archive's comment tree: a comment
// ("t1") with its replies, or a "more" stub naming replies it did not expand.
type redditArchiveNode struct {
	Kind string `json:"kind"`
	Data struct {
		Author    string             `json:"author"`
		Body      string             `json:"body"`
		Score     int                `json:"score"`
		Created   redditArchiveEpoch `json:"created_utc"`
		Retrieved redditArchiveEpoch `json:"retrieved_on"`
		Replies   json.RawMessage    `json:"replies"` // "" for none, else a listing
		Count     int                `json:"count"`   // a "more" stub's replies
	} `json:"data"`
}

// redditArchiveGet asks the archive for path?query and decodes its data. A
// call the archive sheds is asked again after each of redditArchiveWaits; a
// full-text search is asked once, since a search it sheds is matched here
// instead (redditArchiveSearch).
func (h *Harvester) redditArchiveGet(ctx context.Context, path string, query url.Values, data any) error {
	var answer struct {
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	address := redditArchiveBase + path + "?" + query.Encode()
	waits := redditArchiveWaits
	if query.Has("query") {
		waits = nil
	}
	for asked := 0; ; asked++ {
		err := getJSONBody(ctx, h.oa, address, defaultUA, nil, redditArchiveMaxBytes, &answer)
		if err == nil {
			break
		}
		if asked >= len(waits) || !redditArchiveShed(err) {
			return fmt.Errorf("the Reddit archive %s: %w", path, err)
		}
		obs.Logger(ctx).Info("harvest: the Reddit archive shed a call; waiting before asking again",
			"path", path, "wait", waits[asked].String(), obs.FieldErr, err.Error())
		if err := h.nowClock().Sleep(ctx, waits[asked]); err != nil {
			return fmt.Errorf("the Reddit archive %s: %w", path, err)
		}
	}
	if answer.Error != "" {
		return fmt.Errorf("the Reddit archive %s: %s", path, answer.Error)
	}
	if err := json.Unmarshal(answer.Data, data); err != nil {
		return fmt.Errorf("the Reddit archive %s: decode data: %w", path, err)
	}
	return nil
}

// redditArchiveRead answers ask from the archive: the artifact's markdown and
// its partial reason. Empty markdown with a nil error is an ask the archive
// holds no record for; an error is an archive that could not be asked.
func (h *Harvester) redditArchiveRead(ctx context.Context, ask redditArchiveAsk) (markdown, reason string, err error) {
	switch ask.shape {
	case redditShapeThread:
		return h.redditArchiveThread(ctx, ask)
	case redditShapeAbout:
		return h.redditArchiveAbout(ctx, ask)
	case redditShapeSearch:
		return h.redditArchiveSearch(ctx, ask)
	default:
		return h.redditArchiveListing(ctx, ask)
	}
}

// redditArchiveReason is the partial reason of an artifact read from the
// archive: its record of day (when the record carries one), never the live
// page, then what this answer leaves out.
func redditArchiveReason(day string, gaps ...string) string {
	record := "the Arctic Shift archive's record"
	if day != "" {
		record += " of " + day
	}
	return joinReasons(append([]string{"archived copy: " + record + ", not the live Reddit page"}, gaps...)...)
}

func (h *Harvester) redditArchiveThread(ctx context.Context, ask redditArchiveAsk) (string, string, error) {
	var posts []redditArchivePost
	if err := h.redditArchiveGet(ctx, "/api/posts/ids", url.Values{"ids": {"t3_" + ask.post}}, &posts); err != nil {
		return "", "", err
	}
	if len(posts) == 0 {
		return "", "", nil
	}
	post := posts[0]
	tree := url.Values{"link_id": {"t3_" + ask.post}, "limit": {strconv.Itoa(redditArchiveTree)}}
	if ask.comment != "" {
		tree.Set("parent_id", "t1_"+ask.comment)
	}
	var nodes []redditArchiveNode
	if err := h.redditArchiveGet(ctx, "/api/comments/tree", tree, &nodes); err != nil {
		return "", "", err
	}
	var comments []redditComment
	retrieved := post.retrieved()
	deleted, removed, stubs, unexpanded := 0, 0, 0, 0
	var walk func(nodes []redditArchiveNode, level int) error
	walk = func(nodes []redditArchiveNode, level int) error {
		for _, node := range nodes {
			if node.Kind != "t1" {
				stubs++
				unexpanded += node.Data.Count
				continue
			}
			comment := redditComment{
				level:   level,
				author:  node.Data.Author,
				score:   strconv.Itoa(node.Data.Score),
				created: node.Data.Created.day(),
			}
			// The archive keeps the stub Reddit showed in a deleted or removed
			// comment's place when it never saw the comment's own text.
			switch body := strings.TrimSpace(html.UnescapeString(node.Data.Body)); body {
			case "[deleted]":
				comment.status = "deleted"
				deleted++
			case "[removed]":
				comment.status = "removed"
				removed++
			case "":
			default:
				comment.blocks = []string{body}
			}
			comments = append(comments, comment)
			retrieved = max(retrieved, node.Data.Retrieved)
			if len(node.Data.Replies) == 0 || node.Data.Replies[0] != '{' {
				continue
			}
			var replies struct {
				Data struct {
					Children []redditArchiveNode `json:"children"`
				} `json:"data"`
			}
			if err := json.Unmarshal(node.Data.Replies, &replies); err != nil {
				return fmt.Errorf("the Reddit archive /api/comments/tree: decode replies: %w", err)
			}
			if err := walk(replies.Data.Children, level+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(nodes, 0); err != nil {
		return "", "", err
	}

	var gaps []string
	if stubs > 0 {
		gaps = append(gaps, fmt.Sprintf("%d replies behind %d branch(es) the archive did not expand", unexpanded, stubs))
	}
	countLine := fmt.Sprintf(
		"**Comments:** %d stated · %d loaded (%d deleted, %d removed)",
		post.NumComments, len(comments), deleted, removed,
	)
	switch {
	case ask.comment != "":
		gaps = append(gaps, "one comment's branch, as the address asks; the rest of the thread is at the thread's address")
	case stubs == 0 && post.NumComments > len(comments):
		countLine += fmt.Sprintf(" · %d not in the archive's record", post.NumComments-len(comments))
	}
	if len(gaps) > 0 {
		countLine += " · gaps: " + strings.Join(gaps, "; ")
	}
	thread := redditThreadDocument{
		title:      html.UnescapeString(post.Title),
		author:     post.Author,
		score:      strconv.Itoa(post.Score),
		posted:     post.Created.day(),
		permalink:  post.thread(),
		countLine:  countLine,
		body:       strings.TrimSpace(html.UnescapeString(post.Selftext)),
		comments:   comments,
		noComments: "*No comments are in the archive's record.*",
	}
	if post.Subreddit != "" {
		thread.subreddit = "r/" + post.Subreddit
	}
	if !post.IsSelf {
		thread.link = html.UnescapeString(post.URL)
	}
	return thread.markdown(), redditArchiveReason(retrieved.day(), gaps...), nil
}

func (h *Harvester) redditArchiveAbout(ctx context.Context, ask redditArchiveAsk) (string, string, error) {
	var found []struct {
		Name        string             `json:"display_name"`
		Title       string             `json:"title"`
		Description string             `json:"public_description"`
		Subscribers *int               `json:"subscribers"`
		Created     redditArchiveEpoch `json:"created_utc"`
		Retrieved   redditArchiveEpoch `json:"retrieved_on"`
		Meta        struct {
			Posts        *int               `json:"num_posts"`
			Comments     *int               `json:"num_comments"`
			EarliestPost redditArchiveEpoch `json:"earliest_post"`
		} `json:"_meta"`
	}
	if err := h.redditArchiveGet(ctx, "/api/subreddits/search", url.Values{"subreddit": {ask.subreddit}}, &found); err != nil {
		return "", "", err
	}
	for _, subreddit := range found {
		if !strings.EqualFold(subreddit.Name, ask.subreddit) {
			continue // the archive matches by prefix: only the named subreddit is the answer
		}
		var out strings.Builder
		out.WriteString("# r/" + subreddit.Name + "\n\n")
		var facts []string
		if title := html.UnescapeString(subreddit.Title); title != "" {
			facts = append(facts, "**Title:** "+title)
		}
		if subreddit.Subscribers != nil {
			facts = append(facts, "**Subscribers:** "+strconv.Itoa(*subreddit.Subscribers))
		}
		if created := subreddit.Created.day(); created != "" {
			facts = append(facts, "**Created:** "+created)
		}
		if subreddit.Meta.Posts != nil {
			facts = append(facts, "**Archived posts:** "+strconv.Itoa(*subreddit.Meta.Posts))
		}
		if subreddit.Meta.Comments != nil {
			facts = append(facts, "**Archived comments:** "+strconv.Itoa(*subreddit.Meta.Comments))
		}
		if earliest := subreddit.Meta.EarliestPost.day(); earliest != "" {
			facts = append(facts, "**Earliest archived post:** "+earliest)
		}
		out.WriteString(strings.Join(facts, " · ") + "\n")
		if description := strings.TrimSpace(html.UnescapeString(subreddit.Description)); description != "" {
			out.WriteString("\n" + description + "\n")
		}
		return out.String(), redditArchiveReason(subreddit.Retrieved.day()), nil
	}
	return "", "", nil
}

// redditArchivePosts reads up to pages pages of a subreddit's posts, newest
// first, none older than since; query narrows them with the archive's own
// full-text search. more reports that older posts were left unread.
func (h *Harvester) redditArchivePosts(
	ctx context.Context,
	subreddit, query string,
	since time.Time,
	pages int,
) (posts []redditArchivePost, more bool, err error) {
	params := url.Values{"subreddit": {subreddit}, "limit": {strconv.Itoa(redditArchivePage)}, "sort": {"desc"}}
	if query != "" {
		params.Set("query", query)
	}
	if !since.IsZero() {
		params.Set("after", strconv.FormatInt(since.Unix(), 10))
	}
	for page := 0; page < pages; page++ {
		var batch []redditArchivePost
		if err := h.redditArchiveGet(ctx, "/api/posts/search", params, &batch); err != nil {
			return nil, false, err
		}
		posts = append(posts, batch...)
		if len(batch) < redditArchivePage {
			return posts, false, nil
		}
		params.Set("before", strconv.FormatInt(int64(batch[len(batch)-1].Created), 10))
	}
	return posts, true, nil
}

// redditArchiveSince is the start of the window a listing or search asks for:
// Reddit's own default of a day for a "top" listing with none, else all time.
func (h *Harvester) redditArchiveSince(ask redditArchiveAsk) (time.Time, string) {
	window := ask.window
	if window == "" && ask.shape == redditShapeListing && ask.order == redditSortTop {
		window = "day"
	}
	span, ok := redditWindows[window]
	if !ok {
		return time.Time{}, ""
	}
	return h.nowClock().Now().Add(-span), window
}

func (h *Harvester) redditArchiveListing(ctx context.Context, ask redditArchiveAsk) (string, string, error) {
	since, window := h.redditArchiveSince(ask)
	pages := 1
	if ask.order == redditSortTop {
		pages = redditArchivePages
	}
	posts, more, err := h.redditArchivePosts(ctx, ask.subreddit, "", since, pages)
	if err != nil {
		return "", "", err
	}
	if len(posts) == 0 && since.IsZero() {
		return "", "", nil // no record of the subreddit: not an empty listing
	}
	heading, gap := "newest posts", ""
	switch ask.order {
	case redditSortTop:
		heading = "top posts"
		gap = fmt.Sprintf("ranked here by archived score among %d posts", len(posts))
		if more {
			gap += ", the newest of the window; older ones were not ranked"
		}
		sort.SliceStable(posts, func(i, j int) bool { return posts[i].Score > posts[j].Score })
		posts = posts[:min(len(posts), redditArchivePage)]
	case redditSortNew:
	default:
		gap = "newest posts first: the archive keeps no " + cmp.Or(ask.order, "hot") + " ranking"
	}
	if window != "" {
		heading += " of the past " + window
	}
	return redditArchivePostList("r/"+ask.subreddit+" — "+heading, posts), redditArchiveReason("", gap), nil
}

func (h *Harvester) redditArchiveSearch(ctx context.Context, ask redditArchiveAsk) (string, string, error) {
	since, window := h.redditArchiveSince(ask)
	gap := "the archive's own search of r/" + ask.subreddit + ", newest first"
	posts, more, err := h.redditArchivePosts(ctx, ask.subreddit, ask.query, since, 1)
	if err != nil {
		// The archive's full-text search times out under load; the subreddit's
		// newest posts still list, and are matched here on every word.
		searchErr := err
		var newest []redditArchivePost
		if newest, more, err = h.redditArchivePosts(ctx, ask.subreddit, "", since, redditArchivePages); err != nil {
			return "", "", err
		}
		words := strings.Fields(strings.ToLower(ask.query))
		posts = posts[:0]
		for _, post := range newest {
			text := strings.ToLower(html.UnescapeString(post.Title + "\n" + post.Selftext))
			matched := true
			for _, word := range words {
				matched = matched && strings.Contains(text, strings.Trim(word, `"`))
			}
			if matched {
				posts = append(posts, post)
			}
		}
		gap = fmt.Sprintf(
			"every word matched here in the %d newest posts of r/%s, since the archive's own search did not answer (%v)",
			len(newest), ask.subreddit, searchErr,
		)
		if more {
			gap += "; older posts were not searched"
		}
	} else if more {
		gap += fmt.Sprintf("; the %d newest matches, older ones not listed", len(posts))
	}
	if ask.order == redditSortTop {
		sort.SliceStable(posts, func(i, j int) bool { return posts[i].Score > posts[j].Score })
	}
	heading := fmt.Sprintf("r/%s — posts matching %q", ask.subreddit, ask.query)
	if window != "" {
		heading += " in the past " + window
	}
	return redditArchivePostList(heading, posts), redditArchiveReason("", gap), nil
}

// redditArchivePostList renders posts as a numbered list under heading, each
// with its thread address so a reader can ask for the thread next.
func redditArchivePostList(heading string, posts []redditArchivePost) string {
	var out strings.Builder
	out.WriteString("# " + heading + "\n\n")
	out.WriteString(fmt.Sprintf("**Posts:** %d\n\n", len(posts)))
	for index, post := range posts {
		line := fmt.Sprintf("%d. **[%s](%s)**", index+1, redditLinkText(post.Title), post.thread())
		author := post.Author
		if author != "" && !strings.HasPrefix(author, "[") {
			author = "u/" + author
		}
		for _, fact := range []string{
			author,
			fmt.Sprintf("%d points", post.Score),
			fmt.Sprintf("%d comments", post.NumComments),
			post.Created.day(),
		} {
			if fact != "" {
				line += " · " + fact
			}
		}
		out.WriteString(line + "\n")
		if excerpt := redditExcerpt(post.Selftext); excerpt != "" {
			out.WriteString("   " + excerpt + "\n")
		}
	}
	return out.String()
}

// redditLinkText is a post title as markdown link text, on one line.
func redditLinkText(title string) string {
	title = strings.Join(strings.Fields(html.UnescapeString(title)), " ")
	return strings.NewReplacer("[", `\[`, "]", `\]`).Replace(title)
}

// redditExcerpt is the start of a post's own text on one line, cut at a rune.
func redditExcerpt(selftext string) string {
	text := strings.Join(strings.Fields(html.UnescapeString(selftext)), " ")
	if text == "[removed]" || text == "[deleted]" {
		return ""
	}
	if runes := []rune(text); len(runes) > redditArchiveExcerpt {
		return string(runes[:redditArchiveExcerpt]) + "…"
	}
	return text
}
