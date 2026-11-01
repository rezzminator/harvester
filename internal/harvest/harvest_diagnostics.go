package harvest

import (
	"fmt"
	"net/url"
	"strings"
)

// titleDiagnosticMarker is the title diagnostic's own wording. Its kind,
// "ambiguous", is set by its producer (titleGuessResult), never read from
// text: the message quotes the caller's title, which may carry any marker.
const titleDiagnosticMarker = " is a title — use the `search_literature` tool"

// pubMedSearchMarker names a PubMed search/results URL given as an article:
// an invalid input, never an ambiguous title. Its producer sets the kind,
// never the text, which quotes the address.
const pubMedSearchMarker = " is a PubMed search/results URL, not an article"

// isPubMedSearchURL reports whether raw is a PubMed search/results address.
func isPubMedSearchURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Hostname(), "pubmed.ncbi.nlm.nih.gov") {
		return false
	}
	return strings.Contains(strings.ToLower(u.Path), "/search") ||
		strings.Contains(strings.ToLower(u.RawQuery), "term=")
}

// titleGuessResult builds the "that looks like a title, not a fetchable
// identifier" answer fetchUnshared returns for a bare title.Result — the same
// wording whether the title arrived as an explicit `title:` prefix (echoed
// value) or as a bare non-URL string (echoed source). Shared here instead of
// duplicated at both call sites (fetchUnshared, harvest.go).
func titleGuessResult(source, echoed string) Result {
	return Result{
		Source:    source,
		ErrorKind: errorKindAmbiguous,
		Error: fmt.Sprintf(
			"%q"+titleDiagnosticMarker+" to list candidate works (it returns a handle for each), then read the one you pick with `read` (publications). `read` takes web pages in urls and UNAMBIGUOUS identifiers in publications (DOI, arXiv id, PMID, PMCID, ISBN), never a title.",
			echoed,
		),
	}
}

// noteRungOutcome is a reader rung's failure bookkeeping (Jina, defuddle.md):
// a transport error names its kind, a genuine answer keeps the prior error.
// A reader never reports the target's HTTP status — its own 429 or 502 is
// the reader's answer, not the site's — so lastStatus is never its to set.
func noteRungOutcome(err, lastErr error, lastErrorKind string) (string, error) {
	if err != nil {
		return errorKind(err), err
	}
	return lastErrorKind, lastErr
}

// converterNote appends what a static rung's converter answered to message,
// fills kind when nothing more specific already claimed it, and returns the
// converter's error the note quotes for the published failure. A failure (F12)
// is a tool outage, carrying the converter's error; a converter that ran and
// read the document blank (emptyConversionError) is an empty document,
// carrying what it read, never an outage. A wrong-kind PDF and an app-shell
// terminal carry their own tailored message and take neither note; an
// empty-PDF conversion carries its own empty-PDF message, which a failure on
// another rung must never be masked by — or duplicate.
func converterNote(message, kind string, failed, empty error, emptyPDFConvert, wrongPDF, appShellFailure bool) (string, string, string) {
	if wrongPDF || appShellFailure {
		return message, kind, ""
	}
	said := ""
	if failed != nil && !emptyPDFConvert {
		// The fetched bytes are not kept here; download_file fetches them again, unparsed.
		said = failed.Error()
		message += " A conversion step failed on an earlier rung — that is a tool outage on this server, not proof the page has nothing to extract; `download_file` still returns the file's bytes unparsed. The converter's error: " + said + "."
		if kind == "" {
			kind = errorKindConvert
		}
	}
	if empty != nil {
		if !emptyPDFConvert { // the empty-PDF message already says it converted EMPTY
			message += " The converter ran and read the document as blank — it converted EMPTY, not a tool outage: " + empty.Error() + "."
			if kind == "" {
				kind = errorKindEmpty
			}
		}
		if said == "" {
			said = empty.Error()
		}
	}
	return message, kind, said
}
