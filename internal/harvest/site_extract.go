package harvest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"golang.org/x/net/html"

	"github.com/rezzminator/harvester/internal/obs"
)

// Per-site tree extractors. A generic main-content extractor reduces a page
// to its article; where a site's structure is KNOWN — a discussion thread's
// post and nested comments — a structure-aware extractor renders the tree
// instead, and reconciles what it rendered against the count the page itself
// states. Every other site keeps the generic path. To add a site, add one
// entry here: the hosts it owns — or, for software any domain runs, a detect
// function that knows its pages by their markup — and an extract function that answers false
// for any page that is not the shape it knows (a listing, a wall), so that
// page falls through to the generic path — and, when its pages hold loaders
// for the rest of the tree, a loaders function Go follows them by
// (loaders.go) before the extract function reads the page.

// siteExtraction is one structure-aware rendering of a page. partial names
// what the page could not supply (loaders still unexpanded and what they
// hide); "" when the rendering is complete.
// renderMayComplete is set when at least one gap is a loader a browser render
// presses, so the browser rung can close it. apiRecord is set when the
// rendering holds at least one record the site's API answered and the
// extractor proved this thread's (honoured only for an extractor that
// readsSiteAPI). unrendered, answered with ok false, is what an extractor
// that knows the page but could not render it leaves out (its API record did
// not load): the page falls through to another path, which names it. stated,
// set beside unrendered, reads from the content a rung stores the count the
// page states of what was not loaded (booking.go).
type siteExtraction struct {
	markdown          string
	partial           string
	renderMayComplete bool
	apiRecord         bool
	unrendered        string
	stated            func(content string) string
	// comments, when set, is the thread's comment count: loaded of stated.
	comments *commentCount
}

// unknownAuthor stands, in every extractor's rendering, for a post or comment
// that names no author.
const unknownAuthor = "[unknown]"

// unknownPosted stands for the date of a post or comment that names none.
const unknownPosted = unknownAuthor

type siteExtractor struct {
	name  string
	hosts []string
	// detect, when set, claims a page on any host by its markup (a forum
	// engine's generator meta); such an extractor's loaders may only request
	// the page's own host.
	detect func(doc *html.Node) bool
	// paths, when set, narrows the host claim to the addresses it accepts (a
	// host whose pages two extractors split); a loader's request is still
	// judged by the host alone.
	paths   func(page *url.URL) bool
	extract func(doc *html.Node, source *url.URL) (siteExtraction, bool)
	// loaders names the loaders still in a page of this site (loaders.go),
	// in DOM order; nil when the site has none Go can follow.
	loaders func(doc *html.Node, source *url.URL) []pageLoader
	// loaderCap, when set, raises the fetch's loader request cap for this
	// site past loaderRequestCap (a site whose loaders each answer only a
	// few items); the pace and the 429 and wall stops hold as they are.
	loaderCap int
	// pressLoaders: the browser rung may press this site's load-more buttons.
	pressLoaders bool
	// readsSiteAPI: the extractor renders the thread from its site's API, never
	// from the page's markup, so a page it renders from an API record is
	// content even where the site served a wall in its place (harvest.go).
	// Every markup extractor leaves it unset and keeps the wall guard.
	readsSiteAPI bool
}

var siteExtractors = []siteExtractor{
	{
		name:         "reddit-thread",
		hosts:        []string{"reddit.com"},
		extract:      extractRedditThread,
		loaders:      redditLoaders,
		loaderCap:    redditLoaderCap,
		pressLoaders: true,
	},
	{
		name:    "hacker-news-thread",
		hosts:   []string{hnHost},
		extract: extractHNThread,
		loaders: hnLoaders,
	},
	{
		name:    "github-discussion",
		hosts:   []string{githubHost},
		paths:   isGitHubDiscussionAddress,
		extract: extractGitHubDiscussion,
		loaders: githubDiscussionLoaders,
	},
	{
		name:         "github-issue",
		hosts:        []string{githubHost},
		extract:      extractGitHubIssue,
		loaders:      githubLoaders,
		readsSiteAPI: true,
	},
	{
		name:         "stackexchange-listing",
		hosts:        stackExchangeHosts,
		paths:        isStackExchangeListingAddress,
		extract:      extractStackExchangeListing,
		loaders:      stackExchangeListingLoaders,
		readsSiteAPI: true,
	},
	{
		name:         "stackexchange-question",
		hosts:        stackExchangeHosts,
		extract:      extractStackExchangeQuestion,
		loaders:      stackExchangeLoaders,
		readsSiteAPI: true,
	},
	{
		name:         "bluesky-post",
		hosts:        []string{bskyHost, bskyAPIHost},
		extract:      extractBlueskyPost,
		loaders:      bskyLoaders,
		readsSiteAPI: true,
	},
	{
		name:         "x-post",
		hosts:        []string{xHost, twitterHost, xAPIHost},
		paths:        isXStatus,
		extract:      extractXPost,
		loaders:      xLoaders,
		readsSiteAPI: true,
	},
	{
		name:         "mastodon-status",
		detect:       isMastodon,
		extract:      extractMastodonStatus,
		loaders:      mastodonLoaders,
		readsSiteAPI: true,
	},
	{
		name:         "gitlab-item",
		hosts:        []string{gitlabComHost},
		detect:       isGitLab,
		extract:      extractGitLabItem,
		loaders:      gitlabLoaders,
		readsSiteAPI: true,
	},
	{
		name:         "devto-article",
		hosts:        []string{devtoHost},
		extract:      extractDevtoArticle,
		loaders:      devtoLoaders,
		readsSiteAPI: true,
	},
	{
		name:         "lemmy-post",
		hosts:        lemmyHosts,
		detect:       isLemmy,
		extract:      extractLemmyPost,
		loaders:      lemmyLoaders,
		readsSiteAPI: true,
	},
	{
		name:         "substack-post",
		detect:       isSubstack,
		extract:      extractSubstackPost,
		loaders:      substackLoaders,
		readsSiteAPI: true,
	},
	{
		name:         "youtube-video",
		hosts:        []string{youtubeHost},
		paths:        isYouTubeWatch,
		extract:      extractYouTubeVideo,
		loaders:      youtubeLoaders,
		readsSiteAPI: true,
	},
	{
		name:         "steam-app",
		hosts:        []string{steamHost},
		paths:        isSteamApp,
		extract:      extractSteamApp,
		loaders:      steamLoaders,
		readsSiteAPI: true,
	},
	{
		name:         "producthunt-reviews",
		hosts:        []string{"producthunt.com"},
		paths:        isProductHuntReviews,
		extract:      extractProductHuntReviews,
		loaders:      productHuntLoaders,
		readsSiteAPI: true,
	},
	{
		name:         "notion-page",
		hosts:        notionHosts,
		paths:        isNotionPage,
		extract:      extractNotionPage,
		loaders:      notionLoaders,
		readsSiteAPI: true,
	},
	{
		name:    "slashdot-story",
		hosts:   []string{slashdotHost},
		paths:   isSlashdotStory,
		extract: extractSlashdotStory,
		loaders: slashdotLoaders,
	},
	{
		name:         "imdb-reviews",
		hosts:        []string{"imdb.com"},
		paths:        isIMDbReviews,
		extract:      extractIMDbReviews,
		loaders:      imdbLoaders,
		readsSiteAPI: true,
	},
	{
		name:    "lobsters-story",
		hosts:   []string{lobstersHost},
		paths:   isLobstersStory,
		extract: extractLobstersStory,
	},
	{
		name:    "booking-reviews",
		hosts:   []string{"booking.com"},
		paths:   isBookingHotel,
		extract: extractBookingReviews,
	},
	{
		name:    "linkedin-page",
		hosts:   []string{linkedInHost},
		paths:   isLinkedInPage,
		extract: extractLinkedInPage,
	},
	{
		name:    "discourse-topic",
		detect:  isDiscourse,
		extract: extractDiscourseTopic,
		loaders: discourseLoaders,
	},
}

// ownsHost reports whether extractor owns host (lowercased): the host itself
// or any subdomain of it.
func (extractor siteExtractor) ownsHost(host string) bool {
	for _, owned := range extractor.hosts {
		if host == owned || strings.HasSuffix(host, "."+owned) {
			return true
		}
	}
	return false
}

// claims reports whether extractor owns page: by its host, or by its markup.
func (extractor siteExtractor) claims(page *url.URL, doc *html.Node) bool {
	if extractor.ownsHost(strings.ToLower(page.Hostname())) && (extractor.paths == nil || extractor.paths(page)) {
		return true
	}
	return extractor.detect != nil && extractor.detect(doc)
}

// mayRequest reports whether a loader of page may request target: a host the
// extractor owns, or — for an extractor that knows pages by their markup —
// the page's own host.
func (extractor siteExtractor) mayRequest(page, target *url.URL) bool {
	host := strings.ToLower(target.Hostname())
	if extractor.ownsHost(host) {
		return true
	}
	return extractor.detect != nil && (target.Scheme == schemeHTTPS || target.Scheme == schemeHTTP) &&
		host == strings.ToLower(page.Hostname())
}

// SitePressesLoaders reports whether the browser rung may press source's
// load-more buttons: true only when a registered extractor owning the host
// asks for it. Pressing can fire requests or navigation, so every other page
// (and an unparsable URL) is rendered read-only. The rule is the host alone:
// the rung asks before any markup is read, so an extractor that claims pages
// by their markup (detect) may not set pressLoaders — a registry test refuses
// that entry, which would otherwise never press.
func SitePressesLoaders(source string) bool {
	parsed, err := url.Parse(source)
	if err != nil {
		obs.Logger(context.Background()).Warn("harvest: a fetch source could not be parsed; loader pressing left off",
			"source", logSource(source), obs.FieldErr, err.Error())
		return false
	}
	if parsed.Host == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, extractor := range siteExtractors {
		if extractor.pressLoaders && extractor.ownsHost(host) {
			return true
		}
	}
	return false
}

// followForSite follows, in doc, the loaders of the extractor that claims the
// page (loaders.go), and returns what the following left in doc. A nil
// budget (a conversion outside the web ladder) and a page no extractor names
// loaders for leave doc as it is, with nothing reported left.
func (h *Harvester) followForSite(
	ctx context.Context,
	source string,
	doc *html.Node,
	budget *loaderBudget,
) loaderRemainder {
	if budget == nil {
		return loaderRemainder{}
	}
	parsed, err := url.Parse(source)
	if err != nil {
		obs.Logger(ctx).Warn("harvest: a fetch source could not be parsed; no loaders followed",
			"source", logSource(source), obs.FieldErr, err.Error())
		return loaderRemainder{}
	}
	if parsed.Host == "" {
		return loaderRemainder{}
	}
	for _, extractor := range siteExtractors {
		if extractor.loaders == nil || !extractor.claims(parsed, doc) {
			continue
		}
		before := budget.requests
		budget.limit = max(budget.limit, extractor.loaderCap)
		rest := h.followLoaders(ctx, doc, parsed, extractor, budget)
		if budget.requests > before || budget.stopped != "" {
			state := fmt.Sprintf("%d followed, %d failed in the fetch", len(budget.followed), len(budget.failures))
			obs.Logger(ctx).Info("harvest: loaders followed",
				"kind", extractor.name,
				"target", logSource(source),
				"count", budget.requests-before,
				"state", state,
				"reason", budget.stopped)
		}
		return rest
	}
	return loaderRemainder{}
}

// extractForSite runs the extractor that claims the page. ok is false when no
// extractor claims it or the page is not the shape it knows; the extraction
// then carries, in unrendered, what an extractor that knew the page could not
// render. apiRecord survives only from an extractor that readsSiteAPI.
func extractForSite(source string, doc *html.Node) (siteExtraction, string, bool) {
	parsed, err := url.Parse(source)
	if err != nil {
		obs.Logger(context.Background()).Warn("harvest: a fetch source could not be parsed; no site extractor tried",
			"source", logSource(source), obs.FieldErr, err.Error())
		return siteExtraction{}, "", false
	}
	if parsed.Host == "" {
		return siteExtraction{}, "", false
	}
	named := siteExtraction{}
	for _, extractor := range siteExtractors {
		if !extractor.claims(parsed, doc) {
			continue
		}
		extraction, ok := extractor.extract(doc, parsed)
		if ok {
			extraction.apiRecord = extraction.apiRecord && extractor.readsSiteAPI
			return extraction, extractor.name, true
		}
		named.unrendered = joinReasons(named.unrendered, extraction.unrendered)
		if named.stated == nil {
			named.stated = extraction.stated
		}
	}
	return named, "", false
}

// keptAnswers returns the elements named tag in doc: the API answers an
// extractor that reads its site's API keeps in the page (keepAnswer).
func keptAnswers(doc *html.Node, tag string) []*html.Node {
	var found []*html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if isElement(node, tag) {
			found = append(found, node)
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return found
}

// keepAnswer appends one API answer to doc's body as a tag element carrying
// the loader's key, the answer's kind and page number — or, with body nil, a
// dropped loader's mark — so a later conversion replays it like any followed
// loader.
func keepAnswer(doc *html.Node, tag, key, kind string, number int, body []byte) {
	parent := firstElement(doc, "body")
	if parent == nil {
		parent = doc
	}
	node := &html.Node{
		Type: html.ElementNode,
		Data: tag,
		Attr: []html.Attribute{
			{Key: "key", Val: key},
			{Key: "kind", Val: kind},
			{Key: "page", Val: strconv.Itoa(number)},
		},
	}
	if body == nil {
		node.Attr = append(node.Attr, html.Attribute{Key: "dropped", Val: "true"})
	} else {
		node.AppendChild(&html.Node{Type: html.TextNode, Data: string(body)})
	}
	parent.AppendChild(node)
}

// keptPages decodes the paged API answers kept in doc under tag, in page
// order, stopping (logged, naming site) at one that no longer decodes or is
// out of order; dropped reports a dropped loader's mark.
func keptPages[T any](doc *html.Node, tag, site string) (pages []T, dropped bool) {
	for _, node := range keptAnswers(doc, tag) {
		if nodeAttr(node, "dropped") != "" {
			dropped = true
			continue
		}
		var answer T
		if err := json.Unmarshal([]byte(rawText(node)), &answer); err != nil {
			obs.Logger(context.Background()).Warn("harvest: a kept "+site+" answer no longer decodes; left out",
				"page", nodeAttr(node, "page"), obs.FieldErr, err.Error())
			break
		}
		if number, err := strconv.Atoi(nodeAttr(node, "page")); err != nil || number != len(pages) {
			obs.Logger(context.Background()).Warn("harvest: a kept "+site+" answer is out of page order; left out",
				"page", nodeAttr(node, "page"))
			break
		}
		pages = append(pages, answer)
	}
	return pages, dropped
}

// isElement reports an element node with the given (custom) tag name.
func isElement(node *html.Node, tag string) bool {
	return node != nil && node.Type == html.ElementNode && node.Data == tag
}

// firstWithAttr returns the first element under node (node included) whose
// attribute key equals value, not descending into elements stop() refuses.
func firstWithAttr(node *html.Node, key, value string, stop func(*html.Node) bool) *html.Node {
	if node.Type == html.ElementNode && nodeAttr(node, key) == value {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && stop != nil && stop(child) {
			continue
		}
		if found := firstWithAttr(child, key, value, stop); found != nil {
			return found
		}
	}
	return nil
}

// markdownRenderer renders a rich-text subtree (a post body, a comment body)
// as Markdown through html-to-markdown: paragraphs, headings, lists, quotes,
// code, tables, links, with text escaped. Controls, scripts, styles and inert
// templates are not text.
type markdownRenderer struct {
	base *url.URL
	// textOnly, when set, names links the site injects into user text (a
	// highlighted search term); they render as their text alone.
	textOnly func(link *url.URL) bool
}

// markdownConverter is the one converter every markdownRenderer uses; a
// rendering's renderer reaches its link rule through the conversion context.
var markdownConverter = newMarkdownConverter()

type markdownRendererKey struct{}

func newMarkdownConverter() *converter.Converter {
	conv := converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(commonmark.WithHorizontalRule("---"), commonmark.WithListEndComment(false)),
		table.NewTablePlugin(),
		strikethrough.NewStrikethroughPlugin(),
	))
	for _, tag := range []string{"template", "button", "svg", "form"} {
		conv.Register.TagType(tag, converter.TagTypeRemove, converter.PriorityStandard)
	}
	// A link that resolves to nothing (a fragment, a script) or that the site
	// injected (textOnly) renders as its text alone.
	conv.Register.RendererFor("a", converter.TagTypeInline,
		func(ctx converter.Context, w converter.Writer, node *html.Node) converter.RenderStatus {
			renderer, _ := ctx.Value(markdownRendererKey{}).(markdownRenderer)
			if !renderer.linkIsText(nodeAttr(node, "href")) {
				return converter.RenderTryNext
			}
			ctx.RenderChildNodes(ctx, w, node)
			return converter.RenderSuccess
		}, converter.PriorityEarly)
	return conv
}

// linkIsText reports whether a link to href renders as its text alone.
func (renderer markdownRenderer) linkIsText(href string) bool {
	if strings.TrimSpace(href) == "" {
		return false
	}
	resolved := renderer.resolve(href)
	if resolved == "" {
		return true
	}
	if renderer.textOnly == nil {
		return false
	}
	parsed, err := url.Parse(resolved)
	if err != nil {
		obs.Logger(context.Background()).Warn("harvest: a link could not be parsed; kept as a link",
			"href", logSource(resolved), obs.FieldErr, err.Error())
		return false
	}
	return renderer.textOnly(parsed)
}

// blocks renders node as Markdown: one block holding the whole rendering, nil
// when it renders nothing. node is left untouched (the converter edits the
// tree it renders, so it renders a copy).
func (renderer markdownRenderer) blocks(node *html.Node) []string {
	domain := ""
	if renderer.base != nil {
		domain = renderer.base.String()
	}
	out, err := markdownConverter.ConvertNode(cloneTree(node),
		converter.WithDomain(domain),
		converter.WithContext(context.WithValue(context.Background(), markdownRendererKey{}, renderer)))
	if err != nil {
		obs.Logger(context.Background()).Warn("harvest: a rich-text body could not be rendered; left out",
			obs.FieldErr, err.Error())
		return nil
	}
	if text := strings.TrimSpace(string(out)); text != "" {
		return []string{text}
	}
	return nil
}

// cloneTree is a deep copy of node, detached from its parent and siblings.
func cloneTree(node *html.Node) *html.Node {
	clone := &html.Node{
		Type: node.Type, DataAtom: node.DataAtom, Data: node.Data, Namespace: node.Namespace,
		Attr: slices.Clone(node.Attr),
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		clone.AppendChild(cloneTree(child))
	}
	return clone
}

func (renderer markdownRenderer) resolve(href string) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(href), "javascript:") {
		return ""
	}
	parsed, err := url.Parse(href)
	if err != nil {
		obs.Logger(context.Background()).Warn("harvest: a link's href could not be parsed; left unresolved",
			"href", logSource(href), obs.FieldErr, err.Error())
		return href
	}
	if renderer.base != nil {
		return renderer.base.ResolveReference(parsed).String()
	}
	return parsed.String()
}

func wrapInline(text, mark string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return mark + text + mark
}

// rawText is node's text with its whitespace intact (code, preformatted).
func rawText(node *html.Node) string {
	return strings.Join(textNodes(node), "")
}

// textNodes is every text node under node, in document order, untouched.
func textNodes(node *html.Node) []string {
	var parts []string
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			parts = append(parts, current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return parts
}

// prefixLines prefixes every line of text; blank lines get blankPrefix.
func prefixLines(text, prefix, blankPrefix string) string {
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if strings.TrimSpace(line) == "" {
			lines[index] = blankPrefix
		} else {
			lines[index] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

// indentAfterFirst indents every line after the first; blank lines stay blank.
func indentAfterFirst(text, indent string) string {
	lines := strings.Split(text, "\n")
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) != "" {
			lines[index] = indent + lines[index]
		}
	}
	return strings.Join(lines, "\n")
}
