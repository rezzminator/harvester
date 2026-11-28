package harvest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"golang.org/x/net/html"

	"github.com/rezzminator/harvester/internal/obs"
)

// Reaching past a Mastodon context's cap. An instance serves an
// unauthenticated reader a bounded context (60 descendants, depth first), so a
// busy status's later replies are cut. The extractor reaches more through
// public, same-host means only, each request through loaders.go's budget:
//   - a loaded status stating more replies than are loaded under it gets its
//     own context (/api/v1/statuses/{id}/context), a subtree with its own cap;
//   - one still short, when it is the page host's own status, gets its
//     ActivityPub replies collection (its uri + /replies, Accept
//     application/activity+json), paged by first/next: each reply there that
//     is the host's own and not yet loaded is read as an API record
//     (/api/v1/statuses/{id}), then treated like any loaded status.
//
// A reply hosted on another instance appears in a collection as a remote uri;
// it counts as loaded when the host already served it in some context (the
// same uri). The host's lookup of a remote uri (search) needs a login, so a
// remote reply the host never served is read from its own instance's public
// API, unauthenticated, when its uri is a Mastodon status address there
// (/api/v1/statuses/{id}, proved by its uri); and a remote status the host
// holds only as a stub — stating more replies than any answer holds under it
// — gets its context from its own instance (/api/v1/statuses/{id}/context).
// Another instance's ids are not the host's: what it answers is loaded under
// each status's uri, its parent resolved by uri (readRemote). At most
// mastodonRemoteReads requests of a fetch go to other instances, every
// Mastodon request is fetchBounded (loaders.go), and a remote reply still
// unread is named, never dropped.

const (
	// mastodonKindReply is a reply's API record, found in a replies collection.
	mastodonKindReply = "reply"
	// mastodonKindReplies is one page of a status's ActivityPub replies
	// collection (or the collection itself, embedding its first page).
	mastodonKindReplies   = "replies"
	mediaTypeActivityJSON = "application/activity+json"
	// mastodonKindRemoteReply is a remote reply's record on its own instance,
	// mastodonKindRemoteCtx a remote status's context there.
	mastodonKindRemoteReply = "remote-reply"
	mastodonKindRemoteCtx   = "remote-context"
	// mastodonRemoteReads bounds the requests one fetch sends to instances
	// other than the page's host.
	mastodonRemoteReads = 40
	mastodonRemoteState = "remote, not read from its own instance (its request failed, or the fetch's bound " +
		"on reads of other instances came first; the loader note names a failure)"
	mastodonRemoteNoAPI = "remote, not fetched (its address is not a Mastodon status, so its own instance's " +
		"API is not known)"
)

// mastodonRemoteAPIRe is the path of a status's record or context on any
// instance: the only requests a Mastodon page's loaders send off its host.
var mastodonRemoteAPIRe = regexp.MustCompile(`^/api/v1/statuses/\d+(?:/context)?$`)

// isMastodonRemoteAPI reports a target a Mastodon page's loader may request on
// another instance: a status's record or context, over https, unqueried.
func isMastodonRemoteAPI(target *url.URL) bool {
	return target.Scheme == schemeHTTPS && target.RawQuery == "" &&
		mastodonRemoteAPIRe.MatchString(target.EscapedPath())
}

// mastodonHome is a status uri's own instance and the status's id there;
// false for a uri that is no Mastodon status address over https.
func mastodonHome(uri string) (origin, id string, ok bool) {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != schemeHTTPS || parsed.Host == "" {
		return "", "", false
	}
	id, ok = mastodonStatusID(parsed.Path)
	return parsed.Scheme + "://" + parsed.Host, id, ok
}

// readFromHome reports a status read from its own instance, loaded under its
// uri (readRemote), never under a host id.
func readFromHome(status *mastodonStatus) bool { return strings.Contains(status.ID, "://") }

// mastodonRemoteAnswer is a kept answer of another instance, read once every
// answer of the page's host is.
type mastodonRemoteAnswer struct {
	kind, target string
	body         []byte
}

// readRemote adds to found the statuses other instances answered, each under
// its uri as its id, the status it answers named by uri and resolved to the id
// that status is loaded under; a status already loaded (the same uri) is not
// added again. A remote reply answers the status whose collection named it; a
// context's descendants answer, by that instance's ids, its subject or each
// other.
func (state *mastodonPage) readRemote(answers []mastodonRemoteAnswer) {
	loaded := map[string]string{} // uri → the id it is loaded under
	home := map[string]string{}   // own instance + " " + id there → uri
	note := func(status *mastodonStatus) {
		if _, seen := loaded[status.URI]; !seen {
			loaded[status.URI] = status.ID
		}
		if origin, id, ok := mastodonHome(status.URI); ok {
			home[origin+" "+id] = status.URI
		}
	}
	if state.status != nil {
		note(state.status)
	}
	for index := range state.found {
		note(&state.found[index])
	}
	owner := map[string]string{} // a collection item's uri → the uri of the status it answers
	for _, page := range state.pages {
		collection := page.PartOf
		if collection == "" {
			collection, _, _ = strings.Cut(page.ID, "?")
		}
		for _, raw := range page.Items {
			if uri, _ := apItemURI(raw); uri != "" {
				owner[uri] = strings.TrimSuffix(collection, "/replies")
			}
		}
	}
	add := func(status mastodonStatus, parent string) {
		if _, seen := loaded[status.URI]; seen || status.URI == "" {
			return
		}
		status.ID, status.InReplyToID = status.URI, nil
		if origin, _, ok := mastodonHome(status.URI); ok && status.Account != nil &&
			status.Account.Acct != "" && !strings.Contains(status.Account.Acct, "@") {
			// Its own instance names a local account without its domain.
			account := *status.Account
			account.Acct += "@" + strings.TrimPrefix(origin, "https://")
			status.Account = &account
		}
		if parent != "" {
			if id, ok := loaded[parent]; ok {
				parent = id
			}
			status.InReplyToID = &parent
		}
		state.found = append(state.found, status)
		note(&state.found[len(state.found)-1])
	}
	for _, answer := range answers {
		var err error
		switch answer.kind {
		case mastodonKindRemoteReply:
			var reply mastodonStatus
			if err = json.Unmarshal(answer.body, &reply); err == nil {
				add(reply, owner[reply.URI])
			}
		case mastodonKindRemoteCtx:
			var thread mastodonContext
			if err = json.Unmarshal(answer.body, &thread); err == nil {
				// The context's subject, by the instance's id its API address
				// names: /api/v1/statuses/{id}/context.
				subject, id := "", ""
				if target, parseErr := url.Parse(strings.TrimSuffix(answer.target, "/context")); parseErr == nil {
					id = path.Base(target.Path)
					subject = home[target.Scheme+"://"+target.Host+" "+id]
				}
				local := map[string]string{id: subject}
				for index := range thread.Descendants {
					local[thread.Descendants[index].ID] = thread.Descendants[index].URI
				}
				for _, reply := range thread.Descendants {
					parent := subject
					if reply.InReplyToID != nil && local[*reply.InReplyToID] != "" {
						parent = local[*reply.InReplyToID]
					}
					add(reply, parent)
				}
			}
		}
		if err != nil {
			obs.Logger(context.Background()).Warn("harvest: a kept Mastodon answer of another instance no longer "+
				"decodes; left out", "kind", answer.kind, obs.FieldErr, err.Error())
			state.undecoded++
		}
	}
}

// remoteKept counts the answers of other instances the page already holds
// (kept or dropped): each spent one of mastodonRemoteReads.
func (state mastodonPage) remoteKept() int {
	count := 0
	for _, keys := range []map[string]bool{state.kept, state.dropped} {
		for key := range keys {
			if !strings.HasPrefix(strings.TrimPrefix(key, mastodonKey("")), state.origin+"/") {
				count++
			}
		}
	}
	return count
}

// apRepliesPage is an ActivityPub replies collection or one of its pages.
type apRepliesPage struct {
	ID     string            `json:"id"`
	Type   string            `json:"type"`
	PartOf string            `json:"partOf"`
	Next   string            `json:"next"`
	First  json.RawMessage   `json:"first"`
	Items  []json.RawMessage `json:"items"`
}

// apItemURI reads a collection item: a remote reply's uri, or the host's own
// reply as its Note object (its id, and the uri it answers).
func apItemURI(raw json.RawMessage) (uri, inReplyTo string) {
	if err := json.Unmarshal(raw, &uri); err == nil {
		return uri, ""
	}
	var note struct {
		ID        string `json:"id"`
		InReplyTo string `json:"inReplyTo"`
	}
	if err := json.Unmarshal(raw, &note); err != nil {
		return "", ""
	}
	return note.ID, note.InReplyTo
}

// firstPage is the collection's first page: embedded (its target and itself)
// or named by its target only.
func (page apRepliesPage) firstPage() (target string, embedded *apRepliesPage) {
	if len(page.First) == 0 {
		return "", nil
	}
	if err := json.Unmarshal(page.First, &target); err == nil {
		return target, nil
	}
	var first apRepliesPage
	if err := json.Unmarshal(page.First, &first); err != nil || first.ID == "" {
		return "", nil
	}
	return first.ID, &first
}

// repliesCollection is the replies collection address of a status's uri.
func repliesCollection(uri string) string { return uri + "/replies" }

// isOwn reports a uri on the page's own host.
func (state mastodonPage) isOwn(uri string) bool {
	parsed, err := url.Parse(uri)
	return err == nil && parsed.Scheme+"://"+parsed.Host == state.origin
}

// mastodonTree is the loaded thread: the replies in load order, once each, and
// the remote replies the collections named that no loaded status is.
type mastodonTree struct {
	replies  []*mastodonStatus
	byID     map[string]*mastodonStatus
	byURI    map[string]bool
	children map[string]int
	// unparented counts the replies served without a parent id, kept under the page's status.
	unparented int
}

func (state mastodonPage) tree() mastodonTree {
	tree := mastodonTree{byID: map[string]*mastodonStatus{}, byURI: map[string]bool{}, children: map[string]int{}}
	if state.status != nil {
		tree.byID[state.id] = state.status
		tree.byURI[state.status.URI] = true
	}
	for index := range state.found {
		status := &state.found[index]
		if tree.byID[status.ID] != nil || (status.URI != "" && tree.byURI[status.URI]) {
			continue
		}
		tree.byID[status.ID] = status
		tree.byURI[status.URI] = true
		tree.replies = append(tree.replies, status)
		if status.InReplyToID == nil {
			tree.children[state.id]++
			tree.unparented++
			continue
		}
		tree.children[*status.InReplyToID]++
	}
	return tree
}

// short lists the loaded statuses stating more replies than are loaded under
// them, the page's status first.
func (tree mastodonTree) short(root *mastodonStatus) []*mastodonStatus {
	var short []*mastodonStatus
	for _, status := range append([]*mastodonStatus{root}, tree.replies...) {
		if status.RepliesCount != nil && tree.children[status.ID] < *status.RepliesCount {
			short = append(short, status)
		}
	}
	return short
}

// collectionItems walks a status's replies collection as far as its pages are
// kept: the item uris read (with the uri each answers, "" for a bare uri) and
// the target of the first page not yet kept ("" when the walk is complete).
func (state mastodonPage) collectionItems(status *mastodonStatus) (items [][2]string, unread string) {
	target, seen := repliesCollection(status.URI), map[string]bool{}
	for target != "" && !seen[target] {
		seen[target] = true
		page, ok := state.pages[target]
		if !ok {
			return items, target
		}
		for _, raw := range page.Items {
			if uri, parent := apItemURI(raw); uri != "" {
				items = append(items, [2]string{uri, parent})
			}
		}
		target = page.Next
		if first, _ := page.firstPage(); first != "" {
			target = first
		}
	}
	return items, ""
}

// replyLoaders names what the thread still lacks past the page status's own
// context: each short status's context, then — for the host's own status —
// its collection's next page and the records of the replies the collection
// named and no answer holds, its own-host ones from the host and remote ones
// from their own instance; for a remote status, its context on its own
// instance. Requests to other instances stop at mastodonRemoteReads.
func (state mastodonPage) replyLoaders(doc *html.Node) []pageLoader {
	tree := state.tree()
	// elsewhere holds the requests to other instances, sent after every
	// request to the host: one slow or refusing instance never comes before
	// the host's own replies.
	var loaders, elsewhere []pageLoader
	remoteLeft := mastodonRemoteReads - state.remoteKept()
	remote := func(kind string, subject *mastodonStatus, target, label string) {
		key := mastodonKey(target)
		if state.kept[key] || state.dropped[key] || remoteLeft <= 0 {
			return
		}
		remoteLeft--
		elsewhere = append(elsewhere, state.loader(doc, kind, subject, target, label, mediaTypeJSON))
	}
	for _, status := range tree.short(state.status) {
		if _, kept := state.contexts[status.ID]; !kept && status.ID != state.id && !readFromHome(status) {
			target := state.origin + "/api/v1/statuses/" + status.ID + "/context"
			loaders = append(loaders, state.loader(doc, mastodonKindCtx, status, target,
				"a reply's context (its replies)", mediaTypeJSON))
			continue
		}
		if !state.isOwn(status.URI) {
			if origin, id, ok := mastodonHome(status.URI); ok {
				remote(mastodonKindRemoteCtx, &mastodonStatus{ID: id, URI: status.URI},
					origin+"/api/v1/statuses/"+id+"/context", "a remote status's context on its own instance")
			}
			continue
		}
		items, unread := state.collectionItems(status)
		if unread != "" {
			loaders = append(loaders, state.loader(doc, mastodonKindReplies, status, unread,
				"a status's ActivityPub replies collection", mediaTypeActivityJSON))
		}
		for _, item := range items {
			if tree.byURI[item[0]] {
				continue
			}
			if !state.isOwn(item[0]) {
				if origin, id, ok := mastodonHome(item[0]); ok {
					remote(mastodonKindRemoteReply, &mastodonStatus{ID: id, URI: item[0]},
						origin+"/api/v1/statuses/"+id, "a remote reply's record on its own instance")
				}
				continue
			}
			id, ok := "", false
			if parsed, err := url.Parse(item[0]); err == nil {
				id, ok = mastodonStatusID(parsed.Path)
			}
			if !ok || tree.byID[id] != nil {
				continue
			}
			loaders = append(loaders, state.loader(doc, mastodonKindReply, status,
				state.origin+"/api/v1/statuses/"+id, "a reply's API record", mediaTypeJSON))
		}
	}
	loaders = append(loaders, elsewhere...)
	var fresh []pageLoader
	for index := range loaders {
		if key := loaders[index].key; !state.dropped[key] && !state.kept[key] {
			fresh = append(fresh, loaders[index])
		}
	}
	return fresh
}

// remoteReplies are the socialPosts of the remote replies the collections
// named that no loaded status is: not read from their own instance, or — an
// address that is no Mastodon status — not fetched.
func (state mastodonPage) remoteReplies(tree mastodonTree) []socialPost {
	var remote []socialPost
	named := map[string]bool{}
	for _, status := range append([]*mastodonStatus{state.status}, tree.replies...) {
		if !state.isOwn(status.URI) {
			continue
		}
		items, _ := state.collectionItems(status)
		for _, item := range items {
			if tree.byURI[item[0]] || named[item[0]] || state.isOwn(item[0]) {
				continue
			}
			named[item[0]] = true
			unavailable := mastodonRemoteState
			if _, _, ok := mastodonHome(item[0]); !ok {
				unavailable = mastodonRemoteNoAPI
			}
			remote = append(remote, socialPost{
				id: item[0], parent: status.ID, link: item[0],
				unavailable: unavailable,
			})
		}
	}
	return remote
}

// loader is one API request of the thread about status (its subject: the
// status whose context or collection is asked, or the parent of the reply
// whose record is).
func (state mastodonPage) loader(doc *html.Node, kind string, status *mastodonStatus, target, label,
	accept string,
) pageLoader {
	key := mastodonKey(target)
	return pageLoader{
		key:     key,
		label:   label,
		method:  http.MethodGet,
		target:  target,
		headers: map[string]string{headerAccept: accept},
		// An instance that does not answer must not hold the fetch past what
		// an MCP client waits.
		fetchBounded: true,
		graft: func(body []byte, contentType string) error {
			if err := state.checkReply(kind, status, target, body, contentType); err != nil {
				return err
			}
			keepAnswer(doc, mastodonAnswerTag, key, kind, 0, body)
			return nil
		},
		drop: func() { keepAnswer(doc, mastodonAnswerTag, key, kind, 0, nil) },
	}
}

// checkReply proves an answer is what its loader asked about status.
func (state mastodonPage) checkReply(kind string, status *mastodonStatus, target string, body []byte,
	contentType string,
) error {
	switch kind {
	case mastodonKindStatus:
		return checkStatus(status.ID, body, contentType)
	case mastodonKindCtx, mastodonKindRemoteCtx:
		return checkContext(status.ID, body, contentType)
	case mastodonKindRemoteReply:
		if err := checkStatus(status.ID, body, contentType); err != nil {
			return err
		}
		var reply mastodonStatus
		_ = json.Unmarshal(body, &reply) // checkStatus decoded it
		if reply.URI != status.URI {
			return fmt.Errorf("answered by the record of %s, not of the reply %s", reply.URI, status.URI)
		}
		return nil
	case mastodonKindReply:
		id := target[strings.LastIndex(target, "/")+1:]
		if err := checkStatus(id, body, contentType); err != nil {
			return err
		}
		var reply mastodonStatus
		_ = json.Unmarshal(body, &reply) // checkStatus decoded it
		if reply.InReplyToID == nil || *reply.InReplyToID != status.ID {
			return fmt.Errorf("answered by status %s, which does not answer %s", id, status.ID)
		}
		return nil
	}
	var page apRepliesPage
	collection := repliesCollection(status.URI)
	if err := json.Unmarshal(body, &page); err != nil || page.Type == "" {
		return fmt.Errorf("answered by a %s body that is not the ActivityPub replies of status %s",
			discourseContentType(contentType), status.ID)
	}
	if owner, _, _ := strings.Cut(page.ID, "?"); owner != collection && page.PartOf != collection {
		return fmt.Errorf("answered by the collection %s, not the replies of status %s", page.ID, status.ID)
	}
	for _, raw := range page.Items {
		if uri, parent := apItemURI(raw); uri == "" || (parent != "" && parent != status.URI) {
			return fmt.Errorf("answered by a replies page holding an item (%s) that does not answer status %s",
				uri, status.ID)
		}
	}
	return nil
}
