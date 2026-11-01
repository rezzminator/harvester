package harvest

import (
	"strings"
	"testing"
)

// TestSocialThreadNamesUnavailableAndUnservedReplies: a reply the API lists
// only as an unavailable mark (Bluesky's notFoundPost and blockedPost) is
// counted apart from the loaded replies and named, and the stated replies
// neither loaded nor marked are named as not served; either flags the
// thread partial. The marks are the AppView's lexicon types
// (app.bsky.feed.defs#notFoundPost, #blockedPost).
func TestSocialThreadNamesUnavailableAndUnservedReplies(t *testing.T) {
	t.Parallel()
	root := bskySocialPost(&bskyNode{Type: bskyThreadType, Post: &bskyPost{URI: "at://did:plc:a/p/root"}}, "")
	root.stated = 5
	thread := socialThread{
		kind:        "bluesky post",
		title:       "a post",
		post:        root,
		repliesRead: true,
		notServed:   "left out",
		replies: []socialPost{
			{id: "at://did:plc:b/p/1", parent: root.id, author: "@user-1", stated: 0},
			bskySocialPost(&bskyNode{Type: "app.bsky.feed.defs#notFoundPost", URI: "at://did:plc:c/p/2"}, root.id),
			bskySocialPost(&bskyNode{Type: "app.bsky.feed.defs#blockedPost", URI: "at://did:plc:d/p/3"}, root.id),
		},
	}
	markdown, partial := thread.render()
	for _, want := range []string{
		"**Replies:** 5 stated · 1 loaded",
		"1 not found (deleted)",
		"1 blocked (by its author or a moderation list)",
		"2 stated repl(ies) not served: left out",
		"- *a reply not found (deleted)* · at://did:plc:c/p/2",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("the rendering lacks %q:\n%s", want, markdown)
		}
	}
	if !strings.Contains(partial, "1 of 5 stated replies loaded") || !strings.Contains(partial, "not served") {
		t.Fatalf("the partial marker does not name the gaps: %q", partial)
	}
	thread.post.stated, thread.replies = 1, thread.replies[:1]
	if _, partial := thread.render(); partial != "" {
		t.Fatalf("a thread loading every stated reply is partial: %q", partial)
	}
	// A site that states no count: the line reads unstated, nothing is
	// reconciled, and replies never read are named without a number.
	thread.statedUnknown = true
	markdown, partial = thread.render()
	if !strings.Contains(markdown, "**Replies:** unstated · 1 loaded\n\n") || partial != "" {
		t.Fatalf("an unstated count is reconciled: partial %q\n%s", partial, markdown)
	}
	thread.replies, thread.repliesRead = nil, false
	markdown, partial = thread.render()
	if !strings.Contains(markdown, "**Replies:** unstated · 0 loaded · gaps: replies not read\n\n") ||
		partial != "bluesky post: 0 replies loaded, none stated — replies not read" {
		t.Fatalf("unread unstated replies are not named: partial %q\n%s", partial, markdown)
	}
}
