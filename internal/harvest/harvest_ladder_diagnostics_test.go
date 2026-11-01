package harvest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestDefuddleRungTransportFailureReachesTerminalDiagnostic pins F11: the
// defuddle rung previously updated neither lastErr nor lastErrorKind on a
// transport failure, the way its Jina sibling already did — so the terminal
// receipt reported an empty ErrorKind for a rung that actually failed to
// reach the network. Watched FAILING before the fix (ErrorKind was "").
func TestDefuddleRungTransportFailureReachesTerminalDiagnostic(t *testing.T) {
	t.Parallel()
	const source = "https://example.test/article"
	const challengeBody = "<html><body>Checking your browser before accessing this site</body></html>"
	client := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "example.test":
			// A challenge answer: sets lastChallenge but touches neither
			// lastErr nor lastErrorKind, isolating the defuddle transport
			// failure below as the only thing that can set ErrorKind.
			return response(r, http.StatusOK, "text/html", challengeBody), nil
		case "defuddle.md":
			return nil, errors.New("connection reset by peer")
		default:
			t.Fatalf("unexpected host %s", r.URL.Host)
			return nil, nil
		}
	})
	challenge := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(r, http.StatusOK, "text/html", challengeBody), nil
	})
	noNetwork := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("no live network in tests")
	})
	h := mustNew(t, Options{
		CacheDir:  t.TempDir(),
		Client:    &http.Client{Transport: client},
		Chrome:    &http.Client{Transport: challenge},
		Jina:      &http.Client{Transport: challenge},
		OA:        &http.Client{Transport: noNetwork},
		Converter: &fakeConverter{},
	})
	got := h.Fetch(context.Background(), source)
	if got.Error == "" {
		t.Fatal("Fetch() over an all-rungs-failing ladder returned no error")
	}
	if got.ErrorKind == "" {
		t.Fatalf(
			"the defuddle rung's transport failure never reached the terminal diagnostic (F11): ErrorKind is empty, Error=%q",
			got.Error,
		)
	}
}

// TestStaticRungConverterAnswerReachesTheFailure pins F12 and the converter's
// own words: a converter failure on the direct/chrome (static) rungs is named
// a tool outage carrying the converter's error, while a converter that ran
// and read the document blank (the EMPTY-text answer, naming what it read) is
// named empty, never an outage — in the ladder's message and in the published
// failure the caller reads.
func TestStaticRungConverterAnswerReachesTheFailure(t *testing.T) {
	t.Parallel()
	const source = "https://example.test/article"
	// The direct rung answers with usable-looking HTML (long enough to pass
	// the thin-page floor) so the ladder reaches the converter at all; the
	// fake converter below then answers every call with its error.
	body := "<html><body>" + strings.Repeat("real article content ", 80) + "</body></html>"
	client := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "defuddle.md" {
			// Walled: defuddle.md never invokes the converter (it treats its
			// own body as already-converted Markdown), so it must fail on
			// its own terms rather than accidentally succeeding and hiding
			// the static rungs' converter answer this test targets.
			return response(r, http.StatusForbidden, "text/html", "<html><body>access denied</body></html>"), nil
		}
		return response(r, http.StatusOK, "text/html", body), nil
	})
	// The other rungs answer, finding nothing: the converter's answer decides the kind.
	missing := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(r, http.StatusNotFound, "application/json", `{}`), nil
	})
	const lead = "Could not download https://example.test/article — try `search_literature` or another URL for an " +
		"alternative source. "
	const rungs = " Rungs tried: direct, chrome-impersonation, jina, defuddle, wayback."
	const advice = " Find another copy at another URL (search_web, when configured) and read it with read " +
		"(urls), or with search_literature and read (publications) if it is a scholarly work."
	for _, test := range []struct {
		name          string
		err           error
		wantError     string
		wantKind      string
		wantPublished string
	}{
		{
			name: "outage",
			err:  errors.New("converter backend crashed"),
			wantError: lead + "A conversion step failed on an earlier rung — that is a tool outage on this server, " +
				"not proof the page has nothing to extract; `download_file` still returns the file's bytes unparsed. " +
				"The converter's error: converter backend crashed." + rungs,
			wantKind: errorKindConvert,
			wantPublished: "The document was retrieved but the converter could not read it: converter backend " +
				"crashed." + rungs + advice,
		},
		{
			name: "empty",
			err: errors.New(`harvestpy worker returned empty markdown (EMPTY-text conversion); ` +
				`the converter read author "A. Writer", title "Quarterly Report"`),
			wantError: lead + "The converter ran and read the document as blank — it converted EMPTY, not a tool " +
				`outage: harvestpy worker returned empty markdown (EMPTY-text conversion); the converter read ` +
				`author "A. Writer", title "Quarterly Report".` + rungs,
			wantKind: errorKindEmpty,
			wantPublished: "The source was retrieved but yielded no readable content (empty after extraction) — " +
				`the converter ran and read it blank: harvestpy worker returned empty markdown (EMPTY-text ` +
				`conversion); the converter read author "A. Writer", title "Quarterly Report".` + rungs + advice,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := mustNew(t, Options{
				CacheDir:  t.TempDir(),
				Client:    &http.Client{Transport: client},
				Chrome:    &http.Client{Transport: client},
				Jina:      &http.Client{Transport: missing},
				OA:        &http.Client{Transport: missing},
				Converter: failingConverter{err: test.err},
			})
			got := h.Fetch(context.Background(), source)
			if got.Error != test.wantError || got.ErrorKind != test.wantKind {
				t.Fatalf("Fetch() error = %q kind %q,\nwant %q kind %q", got.Error, got.ErrorKind, test.wantError, test.wantKind)
			}
			published := h.PublicResult(source, got, false)
			if published.Error != test.wantPublished {
				t.Fatalf("published error = %q,\nwant %q", published.Error, test.wantPublished)
			}
			if again := PublicFailureMessage(published); again != published.Error {
				t.Fatalf("second public pass = %q,\nwant the published text %q", again, published.Error)
			}
		})
	}
}

// TestShortPageNoRungReadsMoreIsKeptThin: a short page the static rung read,
// with no other rung reading more and no wall seen, is the answer, its partial
// naming why, never a failure that drops it.
func TestShortPageNoRungReadsMoreIsKeptThin(t *testing.T) {
	t.Parallel()
	const source = "https://example.test/note"
	const body = "<html><body><p>Gauge notes: the lattice held.</p></body></html>"
	client := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "defuddle.md" {
			return response(r, http.StatusForbidden, "text/html", "<html><body>access denied</body></html>"), nil
		}
		return response(r, http.StatusOK, "text/html", body), nil
	})
	noNetwork := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("no live network in tests")
	})
	h := mustNew(t, Options{
		CacheDir:  t.TempDir(),
		Client:    &http.Client{Transport: client},
		Chrome:    &http.Client{Transport: client},
		Jina:      &http.Client{Transport: noNetwork},
		OA:        &http.Client{Transport: noNetwork},
		Converter: &fakeConverter{},
	})
	got := h.Fetch(context.Background(), source)
	const wantPartial = "a short page (63 characters) no other rung read more of: it may be a script shell or a wall"
	if got.Error != "" || got.Content != body || got.Partial != wantPartial || got.Method != "direct" {
		t.Fatalf("short page = error %q method %q content %q partial %q, want method direct, content %q, partial %q",
			got.Error, got.Method, got.Content, got.Partial, body, wantPartial)
	}
}

// failingConverter answers every call with err: a crashed converter backend
// (F12/F3's sibling failure mode) or a blank document's EMPTY-text answer.
type failingConverter struct{ err error }

func (c failingConverter) Convert(_ context.Context, _, _ string, _ []byte) (string, error) {
	return "", c.err
}
