package harvest

import (
	"strings"
	"testing"
)

// TestPageInlineStartsAtTheOffset: a public result's inline content is the
// item's content without its leading space, from its offset-th character,
// cut to the character cap with the truncation note; its trailing space is
// kept as read.
func TestPageInlineStartsAtTheOffset(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		offset, limit int
		want          string
	}{
		{"leading space dropped", "\n\n  abc\n", 0, 0, "abc\n"},
		{"from the offset", "abcdef", 2, 0, "cdef"},
		{"offset then the cap", "abcdef", 2, 3, "cde" + InlineTruncationNote},
		{"cap exactly the rest", "abcdef", 2, 4, "cdef"},
		{"multi-byte characters", "日本語のテキスト", 3, 2, "のテ" + InlineTruncationNote},
		{"offset counts after the leading space", "  日本語", 1, 0, "本語"},
		{"offset past the end", "abc", 5, 0, ""},
	} {
		if got := pageInline(tc.body, tc.offset, tc.limit); got != tc.want {
			t.Errorf("%s: pageInline(%q, %d, %d) = %q, want %q", tc.name, tc.body, tc.offset, tc.limit, got, tc.want)
		}
	}
	if !strings.Contains(InlineTruncationNote, "offset") {
		t.Errorf("InlineTruncationNote %q does not teach paging by offset", InlineTruncationNote)
	}
}
