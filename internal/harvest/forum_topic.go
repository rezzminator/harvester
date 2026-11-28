package harvest

// forum_topic.go: phpBB and Invision Community topic pages, claimed on any
// host by their markup. The extractor renders the topic's posts with their
// author and date, and its loader follows the topic's next page (rel="next")
// page after page, joining each page's posts in order, up to forumTopicPages
// pages a fetch and within the fetch's loader time budget (loaders.go); the
// page count the topic states is set against the pages read, and every page
// not read is named.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// The forum engines a topic page is read from.
const (
	forumEnginePhpBB    = "phpbb"
	forumEngineInvision = "invision"
)

// forumTopicPages bounds the pages of one topic a fetch reads: the page the
// fetch began on included.
const forumTopicPages = 20

// forumPageMeta marks, in the page's head, each later page grafted in, by
// its page number.
const forumPageMeta = "harvester-forum-page"

var (
	forumPhpBBPostIDRe    = regexp.MustCompile(`^p(\d+)$`)
	forumInvisionPostIDRe = regexp.MustCompile(`^elComment_(\d+)$`)
	// forumInvisionPageRe reads the page number off an Invision topic
	// address: index.php?/topic/{id}-{slug}/page/{n}/ or /topic/…/page/{n}/.
	forumInvisionPageRe = regexp.MustCompile(`/page/(\d+)/?(?:$|[?#&])`)
	// forumPhpBBPostsRe reads the post count phpBB's pagination block
	// states: "68 posts".
	forumPhpBBPostsRe = regexp.MustCompile(`^(\d[\d,.]*)\s+posts?\b`)
)

// forumTopic is one topic page as the extractor and its loader read it.
type forumTopic struct {
	engine string
	id     string
	title  string
	posts  []*html.Node
	// page is the page number the page names itself; statedPages the topic's
	// page count, 0 when the page states none.
	page, statedPages int
	// statedPosts is the post count phpBB states, -1 when not read.
	statedPosts int
	// idFromAddress: no markup of the page names its topic, the id is read
	// off the address it was asked at — no proof for a later page's answer,
	// which a redirect may have served from another topic.
	idFromAddress bool
}

// isForumTopic reports a phpBB or Invision Community topic page.
func isForumTopic(doc *html.Node) bool {
	return forumEngine(doc) != ""
}

// forumEngine names the engine that generated a topic page, "" for any other
// page: phpBB by its <body id="phpbb" class="section-viewtopic">, Invision
// by its body's page app and controller.
func forumEngine(doc *html.Node) string {
	body := firstElement(doc, "body")
	if body == nil {
		return ""
	}
	switch {
	case nodeAttr(body, "id") == "phpbb" && hasClass(body, "section-viewtopic"):
		return forumEnginePhpBB
	case nodeAttr(body, "data-pageapp") == "forums" && nodeAttr(body, "data-pagecontroller") == "topic":
		return forumEngineInvision
	}
	return ""
}

// forumTopicOf reads doc as a topic page; false when it is none or holds no
// post.
func forumTopicOf(doc *html.Node, page *url.URL) (forumTopic, bool) {
	topic := forumTopic{engine: forumEngine(doc), page: 1, statedPosts: -1}
	canonical := forumCanonical(doc, page)
	switch topic.engine {
	case forumEnginePhpBB:
		topic.posts = forumPosts(doc, func(node *html.Node) bool {
			return node.DataAtom == atom.Div && hasClass(node, "post") && forumPhpBBPostIDRe.MatchString(nodeAttr(node, "id"))
		})
		if canonical != nil {
			topic.id = canonical.Query().Get("t")
		}
		if heading := firstClassElement(doc, atom.H2, "topic-title"); heading != nil {
			topic.title = nodeText(heading)
			if topic.id == "" {
				if link := firstElement(heading, "a"); link != nil {
					if parsed, err := url.Parse(nodeAttr(link, "href")); err == nil {
						topic.id = parsed.Query().Get("t")
					}
				}
			}
		}
		if topic.id == "" && page != nil {
			topic.id, topic.idFromAddress = page.Query().Get("t"), true
		}
		if pagination := firstClassElement(doc, atom.Div, "pagination"); pagination != nil {
			topic.page, topic.statedPages, topic.statedPosts = forumPhpBBPagination(pagination)
		}
	case forumEngineInvision:
		topic.posts = forumPosts(doc, func(node *html.Node) bool {
			return node.DataAtom == atom.Article && forumInvisionPostIDRe.MatchString(nodeAttr(node, "id"))
		})
		body := firstElement(doc, "body")
		topic.id = nodeAttr(body, "data-id")
		if topic.id == "" {
			topic.id = nodeAttr(body, "data-pageid")
		}
		if meta := firstWithAttr(doc, "property", "og:title", nil); meta != nil {
			topic.title = strings.TrimSpace(nodeAttr(meta, "content"))
		}
		address := page.String()
		if canonical != nil {
			address = canonical.String()
		}
		if match := forumInvisionPageRe.FindStringSubmatch(address); match != nil {
			topic.page, _ = strconv.Atoi(match[1])
		}
		if pagination := firstClassElement(doc, atom.Ul, "ipsPagination"); pagination != nil {
			topic.statedPages, _ = strconv.Atoi(strings.TrimSpace(nodeAttr(pagination, "data-pages")))
		}
	default:
		return forumTopic{}, false
	}
	if topic.title == "" {
		topic.title = pageTitle(doc)
	}
	return topic, len(topic.posts) > 0
}

// forumCanonical is doc's <link rel="canonical"> resolved against page, nil
// when it names none.
func forumCanonical(doc *html.Node, page *url.URL) *url.URL {
	for _, link := range discoursePageLinks(doc, page, "canonical") {
		return link
	}
	return nil
}

// forumPosts returns the elements under doc is reports as posts, in DOM order,
// not descending into a post.
func forumPosts(doc *html.Node, is func(*html.Node) bool) []*html.Node {
	var posts []*html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Type != html.ElementNode {
				continue
			}
			if is(child) {
				posts = append(posts, child)
				continue
			}
			walk(child)
		}
	}
	walk(doc)
	return posts
}

// forumPhpBBPagination reads phpBB's pagination block: the current page (its
// active item), the page count (the highest page number it lists) and the
// post count its text states, -1 when it states none.
func forumPhpBBPagination(pagination *html.Node) (page, pages, posts int) {
	page, posts = 1, -1
	for child := pagination.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.TextNode {
			if match := forumPhpBBPostsRe.FindStringSubmatch(strings.TrimSpace(child.Data)); match != nil {
				// A board's locale may group thousands with "." ("1.234 posts").
				if count, ok := redditCount(strings.ReplaceAll(match[1], ".", "")); ok {
					posts = count
				}
				break
			}
		}
	}
	for _, item := range elementsByTag(pagination, atom.Li) {
		number, err := strconv.Atoi(strings.TrimSpace(nodeText(item)))
		if err != nil {
			continue
		}
		if hasClass(item, "active") {
			page = number
		}
		pages = max(pages, number)
	}
	return page, max(pages, page), posts
}

// forumPostID is a post's numeric id, "" for an element that is none.
func forumPostID(post *html.Node) string {
	id := nodeAttr(post, "id")
	for _, re := range []*regexp.Regexp{forumPhpBBPostIDRe, forumInvisionPostIDRe} {
		if match := re.FindStringSubmatch(id); match != nil {
			return match[1]
		}
	}
	return ""
}

// forumNextLink is the topic's next page named by doc, resolved against
// page, without its fragment or phpBB session id; nil when doc names none.
func forumNextLink(doc *html.Node, page *url.URL) *url.URL {
	for _, link := range discoursePageLinks(doc, page, relNext) {
		query := link.Query()
		if query.Has("sid") {
			query.Del("sid")
			link.RawQuery = query.Encode()
		}
		return link
	}
	return nil
}

// forumPagesRead is the last page grafted into doc, first when none was.
func forumPagesRead(doc *html.Node, first int) int {
	last := first
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.DataAtom == atom.Meta && nodeAttr(node, "name") == forumPageMeta {
			if number, err := strconv.Atoi(nodeAttr(node, "content")); err == nil {
				last = max(last, number)
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return last
}

// forumLoaders names the topic's next page while the page links one and the
// fetch has read fewer than forumTopicPages of its pages.
func forumLoaders(doc *html.Node, page *url.URL) []pageLoader {
	topic, ok := forumTopicOf(doc, page)
	if !ok {
		return nil
	}
	next := forumNextLink(doc, page)
	last := forumPagesRead(doc, topic.page)
	if next == nil || topic.id == "" || last-topic.page+1 >= forumTopicPages {
		// No topic id: no later page's answer could be proved this topic's
		// (extractForumTopic names the pages not read).
		return nil
	}
	unlink := func() {
		for _, node := range relElements(doc, relNext) {
			detach(node)
		}
	}
	return []pageLoader{{
		key:          "forum-page " + next.String(),
		label:        fmt.Sprintf("next page (page %d)", last+1),
		method:       http.MethodGet,
		target:       next.String(),
		headers:      map[string]string{headerReferer: page.String()},
		fetchBounded: true,
		pageRung:     true,
		graft: func(body []byte, contentType string) error {
			answer, err := html.Parse(bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("answered by a %s body that does not parse as HTML",
					discourseContentType(contentType))
			}
			answered, ok := forumTopicOf(answer, next)
			switch {
			case !ok:
				return fmt.Errorf("answered by a page (%s, titled %q) that is not a %s topic page with posts",
					discourseContentType(contentType), pageTitle(answer), forumEngineName(topic.engine))
			case answered.engine != topic.engine || topic.id == "" || answered.idFromAddress || answered.id != topic.id:
				// Unknown identity fails closed: posts joined in are
				// presented as this topic's.
				return fmt.Errorf("answered by a page (titled %q) not verifiable as this topic", pageTitle(answer))
			case answered.page <= last:
				return fmt.Errorf("answered by page %d of the topic, already read", answered.page)
			}
			have := map[string]bool{}
			for _, post := range topic.posts {
				have[forumPostID(post)] = true
			}
			tail := topic.posts[len(topic.posts)-1]
			added := 0
			for _, post := range answered.posts {
				if have[forumPostID(post)] {
					continue
				}
				detach(post)
				tail.Parent.InsertBefore(post, tail.NextSibling)
				tail = post
				added++
			}
			if added == 0 {
				return fmt.Errorf("answered by page %d (titled %q) holding no post not already read",
					answered.page, pageTitle(answer))
			}
			unlink()
			if following := forumNextLink(answer, next); following != nil {
				discourseLink(doc, relNext, following.String())
			}
			discourseMark(doc, forumPageMeta, strconv.Itoa(answered.page))
			return nil
		},
		drop: unlink,
	}}
}

// forumEngineName names an engine to a reader.
func forumEngineName(engine string) string {
	if engine == forumEnginePhpBB {
		return "phpBB"
	}
	return "Invision"
}

// forumPageSpan renders a page range: "page 3", "pages 1–5".
func forumPageSpan(first, last int) string {
	if first == last {
		return "page " + strconv.Itoa(first)
	}
	return fmt.Sprintf("pages %d–%d", first, last)
}

// extractForumTopic renders a topic: its header, the pages and posts read
// against those the topic states, and every post in page order.
func extractForumTopic(doc *html.Node, page *url.URL) (siteExtraction, bool) {
	topic, ok := forumTopicOf(doc, page)
	if !ok {
		return siteExtraction{}, false
	}
	first := topic.page
	last := forumPagesRead(doc, first)
	read := last - first + 1
	next := forumNextLink(doc, page)

	var gaps []string
	if first > 1 {
		gaps = append(gaps, fmt.Sprintf("%s not read (the fetch began on page %d)", forumPageSpan(1, first-1), first))
	}
	rest := fmt.Sprintf("page %d on", last+1)
	if topic.statedPages > last {
		rest = forumPageSpan(last+1, topic.statedPages)
	}
	switch {
	case next != nil && topic.id == "":
		gaps = append(gaps, fmt.Sprintf("%s not read: the page names no topic id a later page could be "+
			"proved this topic's by", rest))
	case next != nil && read >= forumTopicPages:
		gaps = append(gaps, fmt.Sprintf("%s not read: the harvester reads at most %d pages of a topic a fetch",
			rest, forumTopicPages))
	case next != nil:
		gaps = append(gaps, fmt.Sprintf("%s not read (next page %s not loaded)", rest, next))
	case topic.statedPages > last:
		gaps = append(gaps, fmt.Sprintf("%s stated but no page links them", rest))
	}

	statedPages := "count not stated"
	progress := forumPageSpan(first, last) + " read, the page count not stated"
	if topic.statedPages > 0 {
		statedPages = strconv.Itoa(topic.statedPages)
		progress = fmt.Sprintf("%s of %d read", forumPageSpan(first, last), topic.statedPages)
	}
	pagesLine := fmt.Sprintf("**Pages:** %s stated · %d loaded (%s read)", statedPages, read, forumPageSpan(first, last))
	postsLine := fmt.Sprintf("**Posts:** count not read (the page states no post count) · %d loaded",
		len(topic.posts))
	if topic.statedPosts >= 0 {
		postsLine = fmt.Sprintf("**Posts:** %d stated · %d loaded", topic.statedPosts, len(topic.posts))
		if missing := topic.statedPosts - len(topic.posts); missing > 0 && len(gaps) == 0 {
			gaps = append(gaps, fmt.Sprintf("%d stated post(s) in no page served", missing))
		}
	}
	if len(gaps) > 0 {
		pagesLine += " · gaps: " + strings.Join(gaps, "; ")
	}

	renderer := markdownRenderer{base: page}
	var out strings.Builder
	out.WriteString("# " + topic.title + "\n\n")
	out.WriteString("**Topic:** " + page.String() + "  \n")
	out.WriteString(pagesLine + "  \n" + postsLine)
	out.WriteString("\n\n---\n\n")
	for _, post := range topic.posts {
		out.WriteString(forumRenderPost(topic.engine, post, renderer))
	}
	partial := ""
	if len(gaps) > 0 {
		partial = fmt.Sprintf("%s topic: %s — %s", topic.engine, progress, strings.Join(gaps, "; "))
	}
	return siteExtraction{markdown: out.String(), partial: partial}, true
}

// forumRenderPost renders one post: a heading with its author, date and id,
// then its body.
func forumRenderPost(engine string, post *html.Node, renderer markdownRenderer) string {
	var body, author *html.Node
	if engine == forumEnginePhpBB {
		body = firstClassElement(post, atom.Div, "content")
		if byline := firstClassElement(post, atom.P, "author"); byline != nil {
			author = firstClassElement(byline, atom.A, "username")
			if author == nil {
				author = firstClassElement(byline, atom.A, "username-coloured")
			}
		}
	} else {
		body = firstWithAttr(post, "data-role", "commentContent", nil)
		for _, link := range elementsByTag(post, atom.A) {
			if strings.Contains(nodeAttr(link, "href"), "/profile/") && nodeText(link) != "" {
				author = link
				break
			}
		}
	}
	name := unknownAuthor
	if author != nil {
		name = nodeText(author)
	}
	date := unknownPosted
	if stamp := elementsByTag(post, atom.Time); len(stamp) > 0 && nodeAttr(stamp[0], "datetime") != "" {
		date = discourseDate(nodeAttr(stamp[0], "datetime"))
	}
	var out strings.Builder
	out.WriteString(fmt.Sprintf("## %s · %s · post %s\n\n", name, date, forumPostID(post)))
	if body != nil {
		if blocks := renderer.blocks(body); len(blocks) > 0 {
			out.WriteString(strings.Join(blocks, "\n\n") + "\n\n")
		}
	}
	return out.String()
}
