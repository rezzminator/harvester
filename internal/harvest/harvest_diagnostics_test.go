package harvest

import (
	"errors"
	"strings"
	"testing"
)

func TestTitleGuessResultEchoesGivenValue(t *testing.T) {
	t.Parallel()
	got := titleGuessResult("title:Some Book", "Some Book")
	if got.Source != "title:Some Book" {
		t.Fatalf("Source = %q, want the original source", got.Source)
	}
	if !strings.Contains(got.Error, `"Some Book"`) || !strings.Contains(got.Error, "search_literature") {
		t.Fatalf("Error = %q, want it to echo the value and name search_literature", got.Error)
	}
}

func TestNoteRungOutcomeOnErrorNamesItsKind(t *testing.T) {
	t.Parallel()
	err := errors.New("connection refused")
	gotKind, gotErr := noteRungOutcome(err, nil, "")
	if gotErr != err {
		t.Fatalf("err = %v, want %v", gotErr, err)
	}
	if gotKind != errorKind(err) {
		t.Fatalf("kind = %q, want %q", gotKind, errorKind(err))
	}
}

func TestNoteRungOutcomeOnAnswerKeepsPriorErr(t *testing.T) {
	t.Parallel()
	priorErr := errors.New("earlier failure")
	gotKind, gotErr := noteRungOutcome(nil, priorErr, "connect")
	if gotErr != priorErr || gotKind != "connect" {
		t.Fatalf("err/kind = %v/%q, want the prior values preserved on a reader's answer", gotErr, gotKind)
	}
}

func TestConverterNoteAppendsOnlyWhenNothingMoreSpecificClaimedIt(t *testing.T) {
	t.Parallel()
	base := "base message"
	failed := errors.New("converter backend crashed")
	empty := errors.New("harvestpy worker returned empty markdown (EMPTY-text conversion)")
	const outageNote = " A conversion step failed on an earlier rung — that is a tool outage on this server, not " +
		"proof the page has nothing to extract; `download_file` still returns the file's bytes unparsed. " +
		"The converter's error: converter backend crashed."
	const emptyNote = " The converter ran and read the document as blank — it converted EMPTY, not a tool outage: " +
		"harvestpy worker returned empty markdown (EMPTY-text conversion)."
	for _, tc := range []struct {
		name                                       string
		kind                                       string
		failed, empty                              error
		emptyPDFConvert, wrongPDF, appShellFailure bool
		wantMessage, wantKind, wantSaid            string
	}{
		{"outage", "", failed, nil, false, false, false, base + outageNote, errorKindConvert, failed.Error()},
		{"empty", "", nil, empty, false, false, false, base + emptyNote, errorKindEmpty, empty.Error()},
		{"outage claims the kind", "", failed, empty, false, false, false,
			base + outageNote + emptyNote, errorKindConvert, failed.Error()},
		{"an earlier kind stands", "connect", failed, nil, false, false, false,
			base + outageNote, "connect", failed.Error()},
		// The empty-PDF message names the empty answer: neither note doubles up on it, the converter's words kept.
		{"emptyPDFConvert", "", failed, empty, true, false, false, base, "", empty.Error()},
		// A more specific diagnostic wins: neither note doubles up on or masks it.
		{"wrongPDF", "existing", failed, empty, false, true, false, base, "existing", ""},
		{"appShellFailure", "existing", failed, empty, false, false, true, base, "existing", ""},
		{"no converter answer", "kind", nil, nil, false, false, false, base, "kind", ""},
	} {
		message, kind, said := converterNote(base, tc.kind, tc.failed, tc.empty, tc.emptyPDFConvert, tc.wrongPDF,
			tc.appShellFailure)
		if message != tc.wantMessage || kind != tc.wantKind || said != tc.wantSaid {
			t.Errorf("%s: converterNote = %q, %q, %q; want %q, %q, %q",
				tc.name, message, kind, said, tc.wantMessage, tc.wantKind, tc.wantSaid)
		}
	}
}
