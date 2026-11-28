package harvestmcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/harvest"
)

// truncatedLine is a cut item's status line: the range shown, the total and
// the offset that continues it, and locally the path.
var truncatedLine = regexp.MustCompile(
	`(?m)^truncated: chars (\d+)-(\d+) of (\d+) shown; read this item alone with offset (\d+) to continue` +
		`(, or read the full text at the path)?$`)

// multiBytePage is a long page of one- to four-byte characters, runs of
// blank space and blank lines among them, distinct per item.
func multiBytePage(item, lines int) string {
	var page strings.Builder
	fmt.Fprintf(&page, "# Item %d\n\n", item)
	for line := range lines {
		fmt.Fprintf(&page, "Reading %d of item %d: Ünïcödé gauge — 日本語の記録 ✓ 🌊 level.", line, item)
		if line%7 == 0 {
			page.WriteString("   \n\n")
		} else {
			page.WriteString("\n")
		}
	}
	return page.String()
}

// parsedPage is one item's block of a read answer: its content and, when cut,
// its truncated line's numbers.
type parsedPage struct {
	content                 string
	cut                     bool
	start, end, total, next int
	namesPath               bool
}

// parsePage reads block, an item's block whose status lines are its header,
// its path line when local and its truncated line when cut.
func parsePage(t *testing.T, block string, local bool) parsedPage {
	t.Helper()
	lines := strings.SplitN(block, "\n", 4)
	head := 1
	if local {
		head = 2
	}
	if len(lines) <= head {
		t.Fatalf("block has no content:\n%.300s", block)
	}
	var page parsedPage
	rest := strings.Join(lines[head:], "\n")
	first, after, _ := strings.Cut(rest, "\n")
	if match := truncatedLine.FindStringSubmatch(first); match != nil {
		numbers := make([]int, 4)
		for index := range numbers {
			numbers[index], _ = strconv.Atoi(match[index+1])
		}
		page = parsedPage{
			cut: true, start: numbers[0], end: numbers[1], total: numbers[2], next: numbers[3],
			namesPath: match[5] != "", content: after,
		}
	} else {
		page.content = rest
	}
	return page
}

// pageThrough follows a cut item's offsets alone to its end, each answer in
// budget, and returns the item's content concatenated from its pages.
func pageThrough(t *testing.T, session *mcp.ClientSession, source string, first parsedPage, local bool, budget int) string {
	t.Helper()
	whole, page, pages := first.content, first, 1
	for page.cut {
		if page.next != page.end || page.end <= page.start {
			t.Fatalf("%s: truncated line continues at offset %d after chars %d-%d", source, page.next, page.start, page.end)
		}
		result, text := callText(t, session, toolRead, map[string]any{"urls": []string{source}, "offset": page.next})
		if result.IsError {
			t.Fatalf("%s offset %d: error result %.300s", source, page.next, text)
		}
		if len(text) > budget {
			t.Fatalf("%s offset %d: answer of %d bytes passes the %d budget", source, page.next, len(text), budget)
		}
		next := parsePage(t, text, local)
		if next.cut && (next.start != page.next || next.total != page.total) {
			t.Fatalf("%s offset %d: page shows chars %d-%d of %d", source, page.next, next.start, next.end, next.total)
		}
		whole += next.content
		page = next
		pages++
	}
	if pages < 2 {
		t.Fatalf("%s: one page, want the item paged", source)
	}
	return whole
}

// TestReadAnswerKeepsWithinItsByteBudget: three long multi-byte items read in
// one call answer within output.maxResultBytes, each with a truncated line;
// following each line's offset alone to the end reproduces every item's
// artifact exactly. A short item beside a long one shows whole and gives its
// unused share to the long one.
func TestReadAnswerKeepsWithinItsByteBudget(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	long := []string{"https://example.test/a", "https://example.test/b", "https://example.test/c"}
	bodies := map[string]string{}
	for index, source := range long {
		bodies[source] = multiBytePage(index+1, 1100+index*150) // past the 50,000-character cap too
		seedArtifact(t, cacheDir, source, "", bodies[source])
	}
	short := "# Small page\n\n" + strings.Repeat("The stilling well agrees with the gauge. ", 6)
	seedArtifact(t, cacheDir, "https://example.test/small", "", short)
	ascii := "# Long page\n\n" + strings.Repeat("Every reading is logged twice. ", 4000)
	seedArtifact(t, cacheDir, "https://example.test/ascii", "", ascii)
	for _, test := range []struct {
		name    string
		runtime Runtime
		budget  int
	}{
		{"default budget", Runtime{}, defaultResultBytes},
		{"configured budget", Runtime{MaxResultBytes: 30000}, 30000},
		{"remote", Runtime{Remote: true}, defaultResultBytes},
		{"inline cap under the budget", Runtime{MaxInlineChars: 4000}, defaultResultBytes},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := test.runtime
			runtime.CacheDir = cacheDir
			local := !runtime.Remote
			session := connectHarvesterInProcess(t, newTestService(t, runtime))
			result, text := callText(t, session, toolRead, map[string]any{"urls": long})
			if result.IsError || len(text) > test.budget {
				t.Fatalf("three long items: error=%v, %d bytes for a %d budget", result.IsError, len(text), test.budget)
			}
			if got := len(truncatedLine.FindAllString(text, -1)); got != 3 {
				t.Fatalf("%d truncated lines, want 3:\n%.600s", got, text)
			}
			blocks := strings.Split(text, "\n\n=== ")
			if len(blocks) != 3 {
				t.Fatalf("%d blocks, want 3", len(blocks))
			}
			for index, source := range long {
				first := parsePage(t, "=== "+strings.TrimPrefix(blocks[index], "=== "), local)
				want := strings.TrimSpace(bodies[source])
				if !first.cut || first.start != 0 || first.total != utf8.RuneCountInString(want) || first.namesPath != local {
					t.Fatalf("%s: first page %+v, want cut from 0 of %d, path named %v", source, first,
						utf8.RuneCountInString(want), local)
				}
				got := pageThrough(t, session, source, first, local, test.budget)
				if got != want {
					t.Fatalf("%s: pages concatenated differ from the artifact (%d of %d chars)", source,
						utf8.RuneCountInString(got), utf8.RuneCountInString(want))
				}
				if !local {
					continue
				}
				path := strings.SplitN(blocks[index], "\n", 3)[1]
				artifact, err := os.ReadFile(path)
				if err != nil || !strings.HasSuffix(strings.TrimSpace(string(artifact)), want) {
					t.Fatalf("%s: artifact at %s does not end with the paged content: %v", source, path, err)
				}
			}

			result, text = callText(t, session, toolRead,
				map[string]any{"urls": []string{"https://example.test/small", "https://example.test/ascii"}})
			blocks = strings.Split(text, "\n\n=== ")
			if result.IsError || len(blocks) != 2 || len(text) > test.budget {
				t.Fatalf("short and long: error=%v, %d blocks, %d bytes", result.IsError, len(blocks), len(text))
			}
			if small := parsePage(t, blocks[0], local); small.cut || small.content != strings.TrimSpace(short) {
				t.Errorf("the short item is not whole:\n%s", blocks[0])
			}
			wantLong := parsePage(t, "=== "+blocks[1], local)
			if !wantLong.cut {
				t.Fatalf("the long item is not cut:\n%.300s", blocks[1])
			}
			// Max-min fair: the long item takes what the short one left, the
			// answer filled to within its truncated line's spare digits.
			if inline := runtime.MaxInlineChars; inline == 0 && len(text) < test.budget-32 {
				t.Errorf("short and long answer %d bytes, want the long item to fill the %d budget", len(text), test.budget)
			}
		})
	}
}

// TestReadBudgetSharesAndBoundaries: the budget's arithmetic, each row an
// exact value — whole at the budget, cut one byte under it, equal shares
// with the integer remainder first, a cut never inside a UTF-8 sequence, a
// cut's trailing space left to the next page, a last page from an offset
// whole, the character cap first, and the floor when the lines fill it.
func TestReadBudgetSharesAndBoundaries(t *testing.T) {
	item := func(content string) harvest.Result {
		return harvest.Result{Content: content, Chars: utf8.RuneCountInString(content), Path: "/cache/public/a.md"}
	}
	header := func(n, total int) string {
		return fmt.Sprintf("=== [%d/%d] https://example.test/%d\n/cache/public/a.md\n", n, total, n)
	}
	sources := func(count int) []string {
		out := make([]string, count)
		for index := range out {
			out[index] = fmt.Sprintf("https://example.test/%d", index+1)
		}
		return out
	}
	// reserve is the bytes a truncated line of an item of total chars may take.
	reserve := func(total int) int {
		return 1 + len(fmt.Sprintf("truncated: chars %d-%d of %d shown; read this item alone with offset %d to continue, "+
			"or read the full text at the path", total, total, total, total))
	}
	twoWhole := len(header(1, 2)+"short") + 2 + len(header(2, 2)+strings.Repeat("x", 500))
	for _, test := range []struct {
		name    string
		results []harvest.Result
		view    ReadView
		want    []ReadPage
	}{
		{
			name:    "whole at the budget",
			results: []harvest.Result{item("short"), item(strings.Repeat("x", 500))},
			view:    ReadView{Budget: twoWhole},
			want:    []ReadPage{{Content: "short", End: 5}, {Content: strings.Repeat("x", 500), End: 500}},
		},
		{
			name:    "one byte under: the short item whole, the long one cut",
			results: []harvest.Result{item("short"), item(strings.Repeat("x", 500))},
			view:    ReadView{Budget: twoWhole - 1},
			want: []ReadPage{{Content: "short", End: 5},
				{Content: strings.Repeat("x", 499-reserve(500)), Cut: true}},
		},
		{
			name:    "equal shares, the remainder to the earliest",
			results: []harvest.Result{item(strings.Repeat("a", 900)), item(strings.Repeat("b", 900)), item(strings.Repeat("c", 900))},
			view:    ReadView{Budget: 3*len(header(1, 3)) + 4 + 3*reserve(900) + 302},
			want: []ReadPage{{Content: strings.Repeat("a", 101), End: 101, Cut: true},
				{Content: strings.Repeat("b", 101), End: 101, Cut: true}, {Content: strings.Repeat("c", 100), End: 100, Cut: true}},
		},
		{
			name:    "a cut on a character boundary",
			results: []harvest.Result{item(strings.Repeat("é", 300))},
			view:    ReadView{Budget: len(header(1, 1)) + reserve(300) + 101},
			want:    []ReadPage{{Content: strings.Repeat("é", 50), End: 50, Cut: true}},
		},
		{
			name:    "trailing space goes to the next page",
			results: []harvest.Result{item("abc   " + strings.Repeat("d", 200))},
			view:    ReadView{Budget: len(header(1, 1)) + reserve(206) + 5},
			want:    []ReadPage{{Content: "abc", End: 3, Cut: true}},
		},
		{
			name:    "the last page from an offset is whole",
			results: []harvest.Result{{Content: "the end", Chars: 107, Offset: 100, Path: "/cache/public/a.md"}},
			view:    ReadView{},
			want:    []ReadPage{{Content: "the end", Start: 100, End: 107}},
		},
		{
			name:    "the character cap applies first",
			results: []harvest.Result{item(strings.Repeat("z", 30))},
			view:    ReadView{InlineCap: 10},
			want:    []ReadPage{{Content: strings.Repeat("z", 10), End: 10, Cut: true}},
		},
		{
			name:    "the floor: the lines alone fill the budget",
			results: []harvest.Result{item(strings.Repeat("a", 900)), item(strings.Repeat("b", 900))},
			view:    ReadView{Budget: 2 * len(header(1, 2))},
			want:    []ReadPage{{Cut: true, Withheld: true}, {Cut: true, Withheld: true}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			blocks, failed, pages := RenderReadItems(sources(len(test.results)), test.results, test.view)
			for index, page := range pages {
				want := test.want[index]
				if want.End == 0 && !want.Withheld {
					want.End = utf8.RuneCountInString(want.Content)
				}
				if page != want {
					t.Errorf("page %d = %+v (%d bytes), want %+v (%d bytes)", index+1, page, len(page.Content), want,
						len(want.Content))
				}
				if failed[index] || !utf8.ValidString(blocks[index]) {
					t.Errorf("block %d failed=%v valid UTF-8=%v", index+1, failed[index], utf8.ValidString(blocks[index]))
				}
			}
			answer := strings.Join(blocks, "\n\n")
			if budget := test.view.budget(); len(answer) > budget && !pages[0].Withheld {
				t.Errorf("answer of %d bytes passes the %d budget", len(answer), budget)
			}
			if pages[0].Withheld {
				for index, block := range blocks {
					want := header(index+1, len(blocks)) +
						"size: 900 chars, content withheld: the item headers alone fill this answer's budget; read fewer items per call"
					if block != want {
						t.Errorf("floor block %d = %q, want %q", index+1, block, want)
					}
				}
			}
		})
	}
}

// TestReadOffsetPagesOneItem: offset takes exactly one item, refused by name
// with more, on the local and the remote server; an offset at or past the
// end is that item's error line naming its total; a negative one is refused.
func TestReadOffsetPagesOneItem(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	body := "# Page\n\n" + strings.Repeat("Every reading is logged twice. ", 10)
	total := utf8.RuneCountInString(strings.TrimSpace(body))
	seedArtifact(t, cacheDir, "https://example.test/page", "", body)
	refused := "error: offset takes exactly one item: it pages one item's content; this call sent 2 items, so read them " +
		"without offset or one at a time"
	for _, test := range []struct {
		name    string
		runtime Runtime
		args    map[string]any
		isError bool
		want    string
	}{
		{"two urls", Runtime{}, map[string]any{"urls": []string{"https://example.test/a", "https://example.test/b"}, "offset": 5}, true, refused},
		{"a url and a publication", Runtime{}, map[string]any{"urls": []string{"https://example.test/a"},
			"publications": []string{"10.1038/nature14539"}, "offset": 1}, true, refused},
		{"remote", Runtime{Remote: true}, map[string]any{"urls": []string{"https://example.test/a", "https://example.test/b"}, "offset": 5}, true, refused},
		{"offset 0 with two is no paging", Runtime{}, map[string]any{"urls": []string{"https://example.test/page", "https://example.test/page"}, "offset": 0}, false, ""},
		{"at the end", Runtime{}, map[string]any{"urls": []string{"https://example.test/page"}, "offset": total}, true,
			fmt.Sprintf("=== [1/1] https://example.test/page\nerror: offset %d is at or past the end of this item: its content is %d chars, so offset takes 0 to %d.", total, total, total-1)},
		{"past the end", Runtime{}, map[string]any{"urls": []string{"https://example.test/page"}, "offset": total + 1000}, true,
			fmt.Sprintf("=== [1/1] https://example.test/page\nerror: offset %d is at or past the end of this item: its content is %d chars, so offset takes 0 to %d.", total+1000, total, total-1)},
		{"the last character", Runtime{Remote: true}, map[string]any{"urls": []string{"https://example.test/page"}, "offset": total - 1}, false,
			"=== [1/1] https://example.test/page\n" + strings.TrimSpace(body)[len(strings.TrimSpace(body))-1:]},
		{"a later page", Runtime{Remote: true}, map[string]any{"urls": []string{"https://example.test/page"}, "offset": 8}, false,
			"=== [1/1] https://example.test/page\n" + strings.TrimSpace(body)[8:]},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := test.runtime
			runtime.CacheDir = cacheDir
			session := connectHarvesterInProcess(t, newTestService(t, runtime))
			result, text := callText(t, session, toolRead, test.args)
			if result.IsError != test.isError || test.want != "" && text != test.want {
				t.Fatalf("isError=%v text:\n%s\nwant isError=%v:\n%s", result.IsError, text, test.isError, test.want)
			}
		})
	}
	session := connectHarvesterInProcess(t, newTestService(t, Runtime{CacheDir: cacheDir}))
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: toolRead, Arguments: map[string]any{"urls": []string{"https://example.test/page"}, "offset": -1},
	})
	if err == nil && !result.IsError {
		t.Fatalf("offset -1 answered %q, want it refused", allText(result))
	}
}

// TestReadToolCarriesItsBudgetForClaudeCode: read's tools/list entry carries
// _meta["anthropic/maxResultSizeChars"] equal to the effective budget, local
// and remote, and both schemas carry offset with its floor; a config written
// before output.maxResultBytes behaves as the default.
func TestReadToolCarriesItsBudgetForClaudeCode(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "harvester.config.json")
	if err := os.WriteFile(path, []byte(`{"output":{"maxInlineChars":40000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	older := RuntimeFromConfig(home, loaded.Config.Harvester)
	older.CacheDir = ""
	for _, test := range []struct {
		name    string
		runtime Runtime
		want    float64
	}{
		{"default", Runtime{}, 50000},
		{"configured", Runtime{MaxResultBytes: 120000}, 120000},
		{"remote", Runtime{Remote: true, MaxResultBytes: 30000}, 30000},
		{"a config without the key", older, 50000},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := connectHarvesterInProcess(t, newTestService(t, test.runtime))
			tools, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tool := range tools.Tools {
				if tool.Name != toolRead {
					if _, ok := tool.Meta[metaMaxResultSizeChars]; ok {
						t.Errorf("%s carries the read budget", tool.Name)
					}
					continue
				}
				if got := tool.Meta[metaMaxResultSizeChars]; got != test.want {
					t.Errorf("read _meta[%q] = %v, want %v", metaMaxResultSizeChars, got, test.want)
				}
				schema, _ := tool.InputSchema.(map[string]any)
				properties, _ := schema["properties"].(map[string]any)
				offset, _ := properties["offset"].(map[string]any)
				if offset["type"] != "integer" || offset["minimum"] != 0.0 {
					t.Errorf("read schema offset = %v, want an integer of minimum 0", offset)
				}
				return
			}
			t.Fatal("tools/list has no read tool")
		})
	}
}
