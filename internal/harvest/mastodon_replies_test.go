package harvest

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestMastodonRepliesReachPastTheContext: a status stating more replies than
// its context served reads its ActivityPub replies collection on its own
// host, page by page; the host's own reply found there loads as an API record
// with its own context (its subtree), a remote reply the host already served
// counts as loaded, and one it never served is read from its own instance's
// public API, its account named with that instance's domain; refused there,
// it is named. A remote reply the host holds only as a stub (stating replies
// its host context does not hold) gets its context from its own instance,
// a third server's reply in it included, nested as that instance answered.
//
// remote-reply-status.json and remote-stub-context.json are hand-written, in
// the shape of a Mastodon 4 instance's /api/v1/statuses/{id} and
// /api/v1/statuses/{id}/context answers.
func TestMastodonRepliesReachPastTheContext(t *testing.T) {
	t.Parallel()
	collection := "social.example/@user-0/" + mastoID + "/replies"
	local := "social.example/api/v1/statuses/117117378316307585"
	remoteReply := "remote.example/api/v1/statuses/117117390641172839"
	stub := "117117243837071506"
	collectionRequests := []string{
		"social.example" + mastoAPI, "social.example" + mastoAPI + "/context", collection,
		collection + "?only_other_accounts=true&page=true",
		collection + "?min_id=117117378316307585&only_other_accounts=true&page=true",
		local, remoteReply, local + "/context",
	}
	withCollection := func(t *testing.T, site *socialSite) {
		site.answers["social.example"+mastoAPI] = strings.Replace(site.answers["social.example"+mastoAPI],
			`"replies_count": 7`, `"replies_count": 9`, 1)
		site.answers[collection] = socialFixture(t, "mastodon/replies.json")
		site.answers[collection+"?only_other_accounts=true&page=true"] = socialFixture(t, "mastodon/replies-page.json")
		site.answers[collection+"?min_id=117117378316307585&only_other_accounts=true&page=true"] = socialFixture(
			t, "mastodon/replies-page-2.json")
		site.answers[local] = socialFixture(t, "mastodon/local-reply-status.json")
		site.answers[local+"/context"] = socialFixture(t, "mastodon/local-reply-context.json")
		site.answers[remoteReply] = socialFixture(t, "mastodon/remote-reply-status.json")
	}
	// past the bound: a collection naming one remote reply more than a fetch
	// reads from other instances, each on its own placeholder instance id.
	farIDs := make([]string, mastodonRemoteReads+1)
	farRequests := []string{
		"social.example" + mastoAPI, "social.example" + mastoAPI + "/context", collection,
		collection + "?only_other_accounts=true&page=true",
	}
	for index := range farIDs {
		farIDs[index] = fmt.Sprintf("9000000000000%05d", index)
		if index < mastodonRemoteReads {
			farRequests = append(farRequests, "far.example/api/v1/statuses/"+farIDs[index])
		}
	}
	pastTheBound := func(t *testing.T, site *socialSite) {
		site.answers["social.example"+mastoAPI] = strings.Replace(site.answers["social.example"+mastoAPI],
			`"replies_count": 7`, fmt.Sprintf(`"replies_count": %d`, 7+len(farIDs)), 1)
		site.answers[collection] = socialFixture(t, "mastodon/replies.json")
		var items []string
		record := socialFixture(t, "mastodon/remote-reply-status.json")
		for _, id := range farIDs {
			uri := "https://far.example/@user-far/" + id
			items = append(items, `"`+uri+`"`)
			site.answers["far.example/api/v1/statuses/"+id] = strings.ReplaceAll(strings.ReplaceAll(record,
				"https://remote.example/@user-12/117117390641172839", uri), `"117117390641172839"`, `"`+id+`"`)
		}
		site.answers[collection+"?only_other_accounts=true&page=true"] = `{"id": "https://` + collection +
			`?only_other_accounts=true&page=true", "type": "CollectionPage", "partOf": "https://` + collection +
			`", "items": [` + strings.Join(items, ", ") + `]}`
	}
	remoteState := "remote, not read from its own instance (its request failed, or the fetch's bound on reads " +
		"of other instances came first; the loader note names a failure)"
	// farRefused: every far.example read refused, none ending the following;
	// the note names the count and the first three.
	var refusals []string
	for _, id := range farIDs[:3] {
		refusals = append(refusals, "a remote reply's record on its own instance (https://far.example/api/v1/statuses/"+
			id+"): HTTP 401")
	}
	farRefused := "mastodon status: 12 of 53 stated replies loaded — 41 repl(ies) " + remoteState + "; 40 loader " +
		"request(s) failed (" + strings.Join(refusals, "; ") + ")"
	cases := []struct {
		name     string
		setup    func(t *testing.T, site *socialSite)
		rendered int
		lines    []string
		partial  string
		requests []string
		// chromeDelivered: the direct client is refused the status's page,
		// which the Chrome rung (sending a browser's Accept) delivers.
		chromeDelivered bool
	}{
		{
			name:     "remote reply read from its own instance",
			setup:    withCollection,
			rendered: 15,
			lines: []string{
				"**Replies:** 15 stated · 15 loaded (9 stated to the post itself, the rest to its replies)",
				"\n- **@user-11** · ",
				"\n  - **@user-0** · ", // the local reply's own reply, from its context
				"\n- **@user-12@remote.example** · 2026-08-18 16:12 UTC · " +
					"[117117390641172839](https://remote.example/@user-12/117117390641172839)\n" +
					"  A remote reply read from its own instance.\n",
			},
			requests: collectionRequests,
		},
		{
			name:            "status page refused to the direct client: its API is still asked on it",
			setup:           withCollection,
			chromeDelivered: true,
			rendered:        15,
			lines: []string{
				"**Replies:** 15 stated · 15 loaded (9 stated to the post itself, the rest to its replies)",
			},
			requests: collectionRequests,
		},
		{
			name: "remote reply its own instance refuses",
			setup: func(t *testing.T, site *socialSite) {
				withCollection(t, site)
				site.status = map[string]int{remoteReply: http.StatusUnauthorized}
			},
			rendered: 14,
			lines: []string{
				"**Replies:** 15 stated · 14 loaded (9 stated to the post itself, the rest to its replies) · 1 " +
					remoteState + " · gaps: 1 repl(ies) " + remoteState,
				"- *a reply " + remoteState + "* · https://remote.example/@user-12/117117390641172839",
			},
			partial: "mastodon status: 14 of 15 stated replies loaded — 1 repl(ies) " + remoteState +
				"; 1 loader request(s) failed (a remote reply's record on its own instance " +
				"(https://remote.example/api/v1/statuses/117117390641172839): HTTP 401)",
			requests: collectionRequests,
		},
		{
			name:     "remote replies past the fetch's bound on reads of other instances",
			setup:    pastTheBound,
			rendered: 12 + mastodonRemoteReads,
			lines: []string{
				"**Replies:** 53 stated · 52 loaded (48 stated to the post itself, the rest to its replies) · 1 " +
					remoteState + " · gaps: 1 repl(ies) " + remoteState,
				"- *a reply " + remoteState + "* · https://far.example/@user-far/" + farIDs[mastodonRemoteReads],
			},
			partial:  "mastodon status: 52 of 53 stated replies loaded — 1 repl(ies) " + remoteState,
			requests: farRequests,
		},
		{
			name: "remote instances refusing every read: each refusal fails its own read alone",
			setup: func(t *testing.T, site *socialSite) {
				pastTheBound(t, site)
				site.status = map[string]int{}
				for _, id := range farIDs {
					site.status["far.example/api/v1/statuses/"+id] = http.StatusUnauthorized
				}
			},
			rendered: 12,
			lines: []string{
				"**Replies:** 53 stated · 12 loaded (48 stated to the post itself, the rest to its replies) · 41 " +
					remoteState + " · gaps: 41 repl(ies) " + remoteState,
			},
			partial:  farRefused,
			requests: farRequests,
		},
		{
			name: "remote stub read from its own instance's context",
			setup: func(t *testing.T, site *socialSite) {
				site.answers["social.example"+mastoAPI+"/context"] = strings.Replace(
					site.answers["social.example"+mastoAPI+"/context"], `"replies_count": 0`, `"replies_count": 1`, 1)
				site.answers["social.example/api/v1/statuses/"+stub+"/context"] = `{"ancestors": [], "descendants": []}`
				site.answers["remote.example/api/v1/statuses/"+stub+"/context"] = socialFixture(
					t, "mastodon/remote-stub-context.json")
			},
			rendered: 14,
			lines: []string{
				"**Replies:** 14 stated · 14 loaded (7 stated to the post itself, the rest to its replies)",
				"\n- **@user-1@remote.example** · 2026-08-18 15:35 UTC · " +
					"[117117243837071506](https://remote.example/@user-1/117117243837071506)\n",
				"\n  - **@user-20@remote.example** · 2026-08-18 17:00 UTC · " +
					"[117117500000000001](https://remote.example/@user-20/117117500000000001)\n" +
					"    A reply the host holds only as its parent's count.\n",
				"\n    - **@user-21@third.example** · 2026-08-18 17:05 UTC · " +
					"[113000000000000001](https://third.example/@user-21/113000000000000001)\n" +
					"      A reply from a third server.\n",
			},
			requests: []string{
				"social.example" + mastoAPI, "social.example" + mastoAPI + "/context",
				"social.example/api/v1/statuses/" + stub + "/context",
				"remote.example/api/v1/statuses/" + stub + "/context",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			site := mastoSite(t)
			tc.setup(t, site)
			h := site.harvester(t)
			if tc.chromeDelivered {
				h.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					if request.URL.Path == "/@user-0/"+mastoID {
						return response(request, http.StatusForbidden, "text/html", "<html><body>Access denied</body></html>"), nil
					}
					return site.roundTrip(request)
				})}
				h.chrome = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					request = request.Clone(request.Context())
					request.Header.Set(headerAccept, chromeAccept)
					return site.roundTrip(request)
				})}
			}
			result := h.FetchWithOptions(context.Background(), mastoURL, FetchOptions{Refresh: true})
			if result.Error != "" {
				t.Fatalf("the status failed: %q", result.Error)
			}
			if got := mastoRendered(result.Content); len(got) != tc.rendered {
				t.Fatalf("rendered %d replies %v, want %d\n%.4000s", len(got), got, tc.rendered, result.Content)
			}
			for _, text := range tc.lines {
				if !strings.Contains(result.Content, text) {
					t.Fatalf("the artifact lacks %q:\n%.4000s", text, result.Content)
				}
			}
			if result.Partial != tc.partial {
				t.Fatalf("partial:\n got %q\nwant %q", result.Partial, tc.partial)
			}
			if strings.Join(site.requests, "\n") != strings.Join(tc.requests, "\n") {
				t.Fatalf("API requests\n%s\nwant\n%s", strings.Join(site.requests, "\n"), strings.Join(tc.requests, "\n"))
			}
			for index, request := range site.requests {
				if strings.Contains(request, "/replies") &&
					site.headers[index].Get("Accept") != "application/activity+json" {
					t.Fatalf("the collection %s was asked without the ActivityPub Accept: %v", request, site.headers[index])
				}
			}
			again := h.FetchWithOptions(context.Background(), mastoURL, FetchOptions{Refresh: true})
			if again.Content != result.Content {
				t.Fatal("a second harvest of the same status differs")
			}
		})
	}
}
