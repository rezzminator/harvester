package harvest

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestGoogleScholarVersionsPageSuppliesSecondPagePDF(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	withPublicDNSForProviderTest(t)
	var versionRequests int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("cluster") == "123" {
			versionRequests++
			return response(
				r,
				http.StatusOK,
				"text/html",
				`<div class="gs_ri"><h3 class="gs_rt"><a href="https://doi.org/10.1234/provider.fixture">Versions fixture</a></h3><div class="gs_a">A Author - Journal, 2020 - repository.example</div><div class="gs_or_ggsm"><a href="https://repository.test/versions.pdf">[PDF]</a></div></div>`,
			), nil
		}
		return response(
			r,
			http.StatusOK,
			"text/html",
			`<div class="gs_ri"><h3 class="gs_rt"><a href="https://doi.org/10.1234/provider.fixture">Versions fixture</a></h3><div class="gs_a">A Author - Journal, 2020 - repository.example</div><div class="gs_fl"><a href="/scholar?cluster=123">All 2 versions</a></div></div>`,
		), nil
	})}
	resolver := &Resolver{Client: client, GoogleScholarURL: "https://scholar.test"}
	got, err := resolver.googleScholar(context.Background(), "fixture", 4)
	if err != nil || len(got) != 1 || got[0].URL != "https://repository.test/versions.pdf" || versionRequests != 1 {
		t.Fatalf("Scholar versions result = %#v err=%v requests=%d", got, err, versionRequests)
	}
}

func TestGoogleScholarKeepsBibliographicFields(t *testing.T) {
	withPublicDNSForProviderTest(t)
	for _, tc := range []struct{ name, page, title string }{
		{
			name:  "linked title",
			page:  `<div class="gs_ri"><h3 class="gs_rt"><a href="https://doi.org/10.1234/provider.fixture">Fixture article</a></h3><div class="gs_a">A Author - Journal, 2020 - repository.example</div><div class="gs_or_ggsm"><a href="https://repository.example/article.pdf">[PDF]</a></div></div>`,
			title: "Fixture article",
		},
		{
			// a [CITATION] row has no title link; its PDF side link still serves it
			name:  "citation row with a PDF side link",
			page:  `<div class="gs_ri"><h3 class="gs_rt"><span class="gs_ctu"><span class="gs_ct1">[CITATION]</span><span class="gs_ct2">[C]</span></span> Fixture article</h3><div class="gs_a">A Author - Journal, 2020 - repository.example</div><div class="gs_or_ggsm"><a href="https://repository.example/article.pdf">[PDF]</a></div></div>`,
			title: "Fixture article",
		},
	} {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return response(r, http.StatusOK, "text/html", tc.page), nil
		})}
		resolver := &Resolver{Client: client, GoogleScholarURL: "https://scholar.test"}
		got, err := resolver.googleScholar(context.Background(), "fixture", 4)
		if err != nil || len(got) != 1 || got[0].Title != tc.title || got[0].Authors != "A Author" ||
			got[0].Year != 2020 ||
			got[0].URL != "https://repository.example/article.pdf" {
			t.Errorf("%s: Scholar bibliographic fields = %#v err=%v", tc.name, got, err)
		}
	}
}

// TestDOIWithNoOpenCopyAnywhereReadsAsNoOpenCopy: every open-access source
// answers and lists no copy, and Google Scholar, the last fallback, answers a
// page without the DOI. Scholar looked and found nothing — the work has no
// open copy, never a connection that failed and should be retried.
func TestDOIWithNoOpenCopyAnywhereReadsAsNoOpenCopy(t *testing.T) {
	withPublicDNSForProviderTest(t)
	const doi = "10.1234/provider.fixture"
	var scholarAsked int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "scholar.test" {
			scholarAsked++
			return response(r, http.StatusOK, "text/html",
				`<html><body><p>Your search did not match any articles.</p></body></html>`), nil
		}
		return jsonResponse(r, `{}`), nil
	})}
	h := mustNew(t, Options{
		CacheDir:         t.TempDir(),
		Client:           client,
		Chrome:           fixtureTwin(client),
		Jina:             client,
		OA:               fixtureTwin(client),
		Converter:        &fakeConverter{},
		ContactEmail:     "qa@example.test",
		GoogleScholarURL: "https://scholar.test",
		BrowserRung:      browserOff(),
		Clock:            newPacingClock(),
	})
	got := h.fetchKnownID(context.Background(), doi, IdentifierDOI, FetchOptions{})
	public := PublicFailure(doi, got)
	const want = "No open copy of this work could be retrieved: every open-access source and mirror tried failed, " +
		"so the work is likely paywalled, and the harvester never signs in."
	if scholarAsked == 0 || public.ErrorKind != errorKindNoOpenCopy || !strings.HasPrefix(public.Error, want) {
		t.Fatalf("Scholar asked %d time(s); public kind %q, error %q;\nwant kind %q, error starting %q\ninternal: kind %q, %s",
			scholarAsked, public.ErrorKind, public.Error, errorKindNoOpenCopy, want, got.ErrorKind, got.Error)
	}
}
