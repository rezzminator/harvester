package harvestmcp

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rezzminator/harvester/internal/harvest"
)

// ReadView is how a read answer renders its items: the MCP read tool's and
// the harvester command's one rendering (RenderReadItems).
type ReadView struct {
	SizeOnly      bool // include_content:false: a size line stands in for the content
	Remote        bool // the remote server: no item names a server path
	InlineCap     int  // the most content characters one item shows; <= 0 is the default
	Budget        int  // the most UTF-8 bytes the answer's text holds, every block together; <= 0 is the default
	SearchEnabled bool // a web search backend is configured, so a failure may name search_web
}

// defaultResultBytes is output.maxResultBytes's default, the budget of one
// read answer: Claude Code saves a tool result over 50,000 characters to a
// file, and a byte count bounds a character count.
const defaultResultBytes = 50000

// metaMaxResultSizeChars is the tools/list _meta key whose value Claude Code
// takes as a tool's result limit in characters, up to 500,000.
const metaMaxResultSizeChars = "anthropic/maxResultSizeChars"

// budget is the view's answer budget in bytes.
func (view ReadView) budget() int {
	if view.Budget > 0 {
		return view.Budget
	}
	return defaultResultBytes
}

// ReadPage is the part of an item's content a read answer shows.
type ReadPage struct {
	Content    string // the content shown
	Start, End int    // the characters of the item's content shown, [Start, End)
	Cut        bool   // the content goes on past End, the offset that continues it
	Withheld   bool   // the budget's floor: the item shows its size and no content
}

// readItem is one item of a read answer before the budget sets its page.
type readItem struct {
	lines   []string // header, path and status lines
	failed  bool
	paged   bool   // the item shows content: it neither failed nor is a size probe
	content string // the content it may show: its inline content within the view's character cap
	start   int    // the character of the item's content that content starts at (read's offset)
	total   int    // the characters of the item's whole content
	path    bool   // the item names its artifact's path
}

// RenderReadItem is item's text block, n of total in its call, rendered
// alone under view's budget:
//
//	=== [n/N] {item as passed}
//	{absolute artifact path}
//	{status lines: partial, truncated — only when the artifact is not clean; or error}
//	{content, or its size}
//
// It reports whether the item failed. An item that failed with no artifact
// is its header and its error line alone.
func RenderReadItem(n, total int, item string, result harvest.Result, view ReadView) (string, bool) {
	items := []readItem{newReadItem(n, total, item, result, view)}
	blocks, _ := renderReadItems(items, view.budget(), 0)
	return blocks[0], items[0].failed
}

// RenderReadItems is a read answer's blocks, one per source in input order,
// within view's budget together (renderReadItems), with whether each failed
// and the page of content each shows.
func RenderReadItems(sources []string, results []harvest.Result, view ReadView) ([]string, []bool, []ReadPage) {
	items := make([]readItem, len(sources))
	failed := make([]bool, len(sources))
	for index, source := range sources {
		items[index] = newReadItem(index+1, len(sources), source, results[index], view)
		failed[index] = items[index].failed
	}
	blocks, pages := renderReadItems(items, view.budget(), 0)
	return blocks, failed, pages
}

// newReadItem is item's lines up to its content and the content it may show.
func newReadItem(n, total int, item string, result harvest.Result, view ReadView) readItem {
	label := harvest.PublicSourceLabel(item)
	lines := []string{itemHeader(n, total, item)}
	path := result.Path
	if view.Remote {
		path = ""
	}
	if message := itemFailure(label, result, view); message != "" {
		if result.Error == "" && path != "" {
			lines = append(lines, path)
		}
		return readItem{lines: append(lines, "error: "+message), failed: true}
	}
	if path != "" {
		lines = append(lines, path)
	}
	if result.Partial != "" {
		lines = append(lines, "partial: "+strings.Join(harvest.PublicGaps(result.Partial), "; "))
	}
	if view.SizeOnly {
		return readItem{lines: append(lines, sizeLine(result.Chars))}
	}
	content, _ := viewContent(result, view)
	return readItem{
		lines: lines, paged: true, content: content, start: result.Offset, path: path != "",
		total: max(result.Chars, result.Offset+utf8.RuneCountInString(strings.TrimRight(content, trailingSpace))),
	}
}

// fixedReadItem is an item whose block is text alone: a failure the read
// call words itself (a misplaced source).
func fixedReadItem(text string) readItem {
	return readItem{lines: []string{text}, failed: true}
}

const trailingSpace = " \t\r\n"

// page is the item's content cut to at most limit bytes on a character
// boundary, its trailing space left to the next page; limit < 0 is no cut.
func (item readItem) page(limit int) ReadPage {
	text := item.content
	if limit >= 0 && limit < len(text) {
		for limit > 0 && !utf8.RuneStart(text[limit]) {
			limit--
		}
		text = text[:limit]
	}
	if trimmed := strings.TrimRight(text, trailingSpace); trimmed != "" || len(text) == len(item.content) {
		text = trimmed
	}
	end := item.start + utf8.RuneCountInString(text)
	return ReadPage{Content: text, Start: item.start, End: end, Cut: end < item.total}
}

// cost is the bytes page adds to the item's lines: its content and, when it
// is cut, its truncated line.
func (item readItem) cost(page ReadPage) int {
	if !page.Cut {
		return len(page.Content)
	}
	return len(page.Content) + 1 + len(item.truncated(page))
}

// lineReserve is the most bytes the item's truncated line can take.
func (item readItem) lineReserve() int {
	return 1 + len(item.truncated(ReadPage{Start: item.total, End: item.total}))
}

// truncated is the status line of a cut page: the range shown, the total,
// the offset that continues it and, locally, the path.
func (item readItem) truncated(page ReadPage) string {
	line := fmt.Sprintf("truncated: chars %d-%d of %d shown; read this item alone with offset %d to continue",
		page.Start, page.End, item.total, page.End)
	if item.path {
		line += ", or read the full text at the path"
	}
	return line
}

// render is the item's block showing page.
func (item readItem) render(page ReadPage) string {
	lines := item.lines
	switch {
	case !item.paged:
	case page.Withheld:
		lines = append(slices.Clone(lines),
			sizeLine(item.total)+", content withheld: the item headers alone fill this answer's budget; read fewer items per call")
	case page.Cut:
		lines = append(slices.Clone(lines), item.truncated(page), page.Content)
	default:
		lines = append(slices.Clone(lines), page.Content)
	}
	return strings.Join(lines, "\n")
}

func sizeLine(chars int) string { return fmt.Sprintf("size: %d chars", chars) }

// renderReadItems renders items so their blocks, joined by a blank line,
// hold at most budget bytes less reserve (the bytes of what follows them):
// every item whole when they fit; else each item's content gets an equal
// share of what the lines leave, an item shorter than its share giving the
// rest back to the longer ones (max-min fair, request order), each cut item
// naming its range and the offset that continues it. When the lines alone
// leave no content (many items), every item shows its size and no content:
// the one answer that may pass the budget.
func renderReadItems(items []readItem, budget, reserve int) ([]string, []ReadPage) {
	pages := make([]ReadPage, len(items))
	var paged []int
	for index, item := range items {
		if item.paged {
			pages[index] = item.page(-1)
			paged = append(paged, index)
		}
	}
	if answerBytes(items, pages)+reserve <= budget {
		return renderPages(items, pages), pages
	}
	room := budget - reserve - 2*(len(items)-1)
	for _, item := range items {
		room -= len(strings.Join(item.lines, "\n"))
		if item.paged {
			room-- // the line break before the content
		}
	}
	order := slices.Clone(paged)
	slices.SortStableFunc(order, func(a, b int) int {
		return items[a].cost(pages[a]) - items[b].cost(pages[b])
	})
	floor := room <= 0
	for rank, index := range order {
		if floor {
			break
		}
		left := len(order) - rank
		share := room / left
		if cost := items[index].cost(pages[index]); cost <= share {
			room -= cost
			continue
		}
		cut := slices.Clone(order[rank:])
		slices.Sort(cut) // the bytes integer division leaves go first to the earliest
		extra := room - share*left
		for position, cutIndex := range cut {
			bytes := share - items[cutIndex].lineReserve()
			if position < extra {
				bytes++
			}
			pages[cutIndex] = items[cutIndex].page(max(bytes, 0))
			floor = floor || pages[cutIndex].End == pages[cutIndex].Start
		}
		break
	}
	if floor || answerBytes(items, pages)+reserve > budget {
		for _, index := range paged {
			start := items[index].start
			pages[index] = ReadPage{Start: start, End: start, Cut: true, Withheld: true}
		}
	}
	return renderPages(items, pages), pages
}

// renderPages is each item's block showing its page.
func renderPages(items []readItem, pages []ReadPage) []string {
	blocks := make([]string, len(items))
	for index, item := range items {
		blocks[index] = item.render(pages[index])
	}
	return blocks
}

// answerBytes is the bytes of items' blocks joined by blank lines.
func answerBytes(items []readItem, pages []ReadPage) int {
	total := 2 * max(len(items)-1, 0)
	for index, item := range items {
		total += len(item.render(pages[index]))
	}
	return total
}

// viewContent is the content an item shows inline: its stored content cut
// to view's character cap; cut reports the cut.
func viewContent(result harvest.Result, view ReadView) (string, bool) {
	content, cut := strings.CutSuffix(result.Content, harvest.InlineTruncationNote)
	limit := view.InlineCap
	if limit <= 0 {
		limit = defaultInlineChars
	}
	if runes := []rune(content); len(runes) > limit {
		content, cut = string(runes[:limit]), true
	}
	return content, cut
}

// itemHeader opens item's block, n of total in its call.
func itemHeader(n, total int, item string) string {
	return fmt.Sprintf("=== [%d/%d] %s", n, total, harvest.PublicSourceLabel(item))
}

// failedItem is the block of an item that failed with no artifact.
func failedItem(n, total int, item, message string) string {
	return itemHeader(n, total, item) + "\nerror: " + message
}

// thinPageChars is the length under which a page answered with an HTTP error or a challenge is
// that error's receipt, not a document.
const thinPageChars = 500

// itemFailure is why a read item is a failure, or "": the harvester's own
// error, a size probe of an empty extraction, an empty body, or a thin HTTP
// error or challenge page standing in for one.
func itemFailure(label string, result harvest.Result, view ReadView) string {
	if result.Error != "" {
		return harvest.PublicFailureMessage(result)
	}
	hint := harvest.SearchHint(
		view.SearchEnabled,
		"Use `search_web` to find an alternative copy, or `search_literature` if it is a scholarly title.",
		"Use `search_literature` if it is a scholarly title, or read an alternative copy at another URL.",
	)
	// Size probes never kill an empty extraction behind a zero-sized success;
	// this wording is part of the Python scheduler contract.
	if view.SizeOnly {
		if strings.TrimSpace(result.Content) == "" && result.Chars == 0 && result.Bytes == 0 {
			return fmt.Sprintf(
				"Fetched %s but it yielded no readable content (empty after extraction) — nothing to size. %s",
				label, hint)
		}
		// The probe blanks the content, so a thin HTTP error page is judged by its char count.
		if result.HTTPStatus >= http.StatusBadRequest && result.Chars < thinPageChars {
			return harvest.PublicFailureMessage(
				harvest.Result{HTTPStatus: result.HTTPStatus, ErrorKind: result.ErrorKind},
			)
		}
		return ""
	}
	stripped := strings.TrimSpace(result.Content)
	status := result.HTTPStatus
	if stripped == "" {
		if result.ErrorKind != "" || result.Challenge || status >= 400 {
			if message := harvest.PublicFailureMessage(result); message != "" {
				return message
			}
		}
		return fmt.Sprintf(
			"Fetched %s but no readable content could be extracted (JS-rendered or bot-blocked — not retrievable from this datacenter IP). %s",
			label,
			hint,
		)
	}
	// A thin HTTP error page, or one the ladder judged a challenge, is a
	// failure, not a successful body. A challenge word in a 200 body only words
	// an error page's failure: a real short page may name Cloudflare.
	lowerBody := strings.ToLower(stripped)
	challenge := result.Challenge || status >= http.StatusBadRequest && (strings.Contains(lowerBody, "cloudflare") ||
		strings.Contains(lowerBody, "captcha") || strings.Contains(lowerBody, "verify you are human"))
	if len([]rune(stripped)) < thinPageChars && (status >= http.StatusBadRequest || challenge) {
		return harvest.PublicFailureMessage(
			harvest.Result{HTTPStatus: status, ErrorKind: result.ErrorKind, Challenge: challenge},
		)
	}
	return ""
}

// readView is this service's rendering of read items.
func (service *Service) readView(sizeOnly bool) ReadView {
	return ReadView{
		SizeOnly:      sizeOnly,
		Remote:        service.runtime.Remote,
		InlineCap:     service.inlineCap(),
		Budget:        service.resultBudget(),
		SearchEnabled: runtimeSearchEnabled(service.runtime),
	}
}

// inlineCap is output.maxInlineChars from harvester.config.json; an
// unconfigured runtime keeps the default.
func (service *Service) inlineCap() int {
	if service != nil && service.runtime.MaxInlineChars > 0 {
		return service.runtime.MaxInlineChars
	}
	return defaultInlineChars
}

// resultBudget is output.maxResultBytes from harvester.config.json, the
// bytes one read answer holds; an unconfigured runtime keeps the default.
func (service *Service) resultBudget() int {
	if service != nil && service.runtime.MaxResultBytes > 0 {
		return service.runtime.MaxResultBytes
	}
	return defaultResultBytes
}

// textResult is a tool answer of one text: an error result when isError.
func textResult(text string, isError bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: isError}
}

// readError is a read call refused as a whole: one error line.
func readError(err error) *mcp.CallToolResult {
	return textResult("error: "+err.Error(), true)
}

// RenderFind is found as text — the candidates, one block each, and every
// source's status, since with a failed source no candidate is not proof the
// work is absent: the search_literature answer and harvester
// search's output.
func RenderFind(query string, found FindOutput) string {
	candidates, sources := found.Candidates, found.Sources
	statuses := make([]string, 0, len(sources))
	var failed bool
	for _, source := range sources {
		status := fmt.Sprintf("%s %s (%d)", source.Source, source.Status, source.Results)
		if source.Error != "" {
			status += ": " + source.Error
		}
		statuses = append(statuses, status)
		failed = failed || source.Status != harvest.SourceAnswered
	}
	footer := ""
	switch {
	case failed:
		footer = "\n\nsources (the records of a source that did not answer are missing from this answer; retry later for them): " +
			strings.Join(
				statuses,
				"; ",
			)
	case len(statuses) > 0:
		footer = "\n\nsources: " + strings.Join(statuses, "; ")
	}
	if len(candidates) == 0 && failed {
		return fmt.Sprintf("No candidate works found for %q among the sources that answered.", query) + footer
	}
	if len(candidates) == 0 {
		return fmt.Sprintf(
			"No candidate works found for %q. Try search_web (when configured) or a plain web search, or rephrase — a more exact title helps.",
			query,
		) + footer
	}
	blocks := []string{fmt.Sprintf(
		"%d candidate work(s) for %q — pick one and read it with `read`, passing its handle in publications:",
		len(candidates), query,
	)}
	for index, candidate := range candidates {
		values := []string{}
		for _, value := range []string{candidate.Type, candidate.Authors} {
			if value != "" {
				values = append(values, value)
			}
		}
		if candidate.Year != 0 {
			values = append(values, strconv.Itoa(candidate.Year))
		}
		openAccess := "open access: no"
		if candidate.OpenAccess {
			openAccess = "open access: yes"
		}
		lines := []string{
			fmt.Sprintf("=== [%d/%d] %s", index+1, len(candidates), valueOr(candidate.Title, "(untitled)")),
			"handle: " + candidate.Handle,
		}
		if len(values) > 0 {
			lines = append(lines, strings.Join(values, " · "))
		}
		if ids := idsText(candidate.IDs); ids != "" {
			lines = append(lines, "ids: "+ids)
		}
		blocks = append(blocks, strings.Join(append(lines, openAccess), "\n"))
	}
	return strings.Join(blocks, "\n\n") + footer
}

// idsText is ids as "doi 10.…, arxiv 1706.03762", sorted by name.
func idsText(ids map[string]string) string {
	names := make([]string, 0, len(ids))
	for name := range ids {
		names = append(names, name)
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+" "+ids[name])
	}
	return strings.Join(parts, ", ")
}

// renderSearch lists the web results, one block each: title, URL, snippet
// and the engine that found it.
func renderSearch(query string, results []WebResult) string {
	if len(results) == 0 {
		return fmt.Sprintf("No results found for %q. Try different terms or a broader query.", query)
	}
	blocks := []string{fmt.Sprintf(
		"%d result(s) for %q — read the ones you want with `read`, in urls:", len(results), query,
	)}
	for index, result := range results {
		lines := []string{
			fmt.Sprintf("=== [%d/%d] %s", index+1, len(results), valueOr(result.Title, "(untitled)")),
			result.URL,
		}
		if result.Snippet != "" {
			lines = append(lines, result.Snippet)
		}
		if result.Engine != "" {
			lines = append(lines, "engine: "+result.Engine)
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return strings.Join(blocks, "\n\n")
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// assertPublicURL delegates to harvest's own SSRF/scheme chokepoint
// (AssertFetchable) instead of keeping a second, drifting copy of the
// private/internal check here — harvest.assertFetchable is the one
// authority (its suffix list, e.g. .ts.net, is not duplicated in this
// package).
func assertPublicURL(parsed *url.URL) error {
	return harvest.AssertFetchable(parsed.String())
}
