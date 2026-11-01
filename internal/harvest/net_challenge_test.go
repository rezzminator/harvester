package harvest

import (
	"net/http"
	"strings"
	"testing"
)

// TestIsChallengeUsesOneShortDefinitionAtTheBoundary pins F10: the
// Cloudflare-marker gate and the forum-phrase gate must agree on what
// "short" means. A body of exactly 4000 content chars used to read short
// (<=4000) for the first gate but not (<4000) for the second, so a
// forum-wall phrase at exactly that length went unflagged.
func TestIsChallengeUsesOneShortDefinitionAtTheBoundary(t *testing.T) {
	t.Parallel()
	phrase := "prove your humanity"
	body := []byte(strings.Repeat("a", 4000-len(phrase)) + phrase)
	if got := contentChars(string(body)); got != 4000 {
		t.Fatalf("fixture body is %d content chars, want exactly 4000 to sit on the isChallenge boundary", got)
	}
	if !isChallenge(body, http.StatusOK) {
		t.Fatalf("a 4000-char, HTTP 200 body containing a forum wall phrase was not flagged a challenge: "+
			"the Cloudflare-marker gate (<=4000) and the forum-phrase gate disagreed at the boundary: %.80q", body)
	}
}

// TestBareWallWordsNeedCorroboration: "captcha", "cloudflare" and "altcha" are
// also the words of a real page that mentions its CDN or loads a
// cdnjs.cloudflare.com script; they mark a wall only beside a 403/503 or in a
// short body that embeds a captcha widget.
func TestBareWallWordsNeedCorroboration(t *testing.T) {
	t.Parallel()
	shortArticle := `<html><head><script src="https://cdnjs.cloudflare.com/ajax/libs/x.js"></script></head>` +
		`<body><h1>Release notes</h1><p>We moved our CDN to Cloudflare and dropped the captcha on sign-up.</p></body></html>`
	longCatalogue := `<html><head><script src="https://cdnjs.cloudflare.com/ajax/libs/x.js"></script>` +
		`<script src="https://www.google.com/recaptcha/api.js"></script></head><body><table>` +
		strings.Repeat("<tr><td>A catalogue record with its title, authors and year</td></tr>", 120) +
		`</table><div class="altcha-free">mirror list</div></body></html>`
	turnstileWall := `<html><body><h1>One more step</h1><div class="cf-turnstile" data-sitekey="fixture"></div></body></html>`
	altchaWall := `<html><body><form><altcha-widget challengeurl="/c"></altcha-widget></form></body></html>`
	for _, tc := range []struct {
		name   string
		check  func([]byte, int) bool
		body   string
		status int
		want   bool
	}{
		{"web: short 200 page naming its CDN", isChallenge, shortArticle, http.StatusOK, false},
		{"web: the same page under a 403", isChallenge, shortArticle, http.StatusForbidden, true},
		{"web: short 200 Turnstile interstitial", isChallenge, turnstileWall, http.StatusOK, true},
		{"mirror: short 200 page naming its CDN", doiMirrorChallenge, shortArticle, http.StatusOK, false},
		{"mirror: long 200 catalogue loading cdnjs and reCAPTCHA", doiMirrorChallenge, longCatalogue, http.StatusOK, false},
		{"mirror: the same catalogue under a 503", doiMirrorChallenge, longCatalogue, http.StatusServiceUnavailable, true},
		{"mirror: altcha widget", doiMirrorChallenge, altchaWall, http.StatusOK, true},
	} {
		if got := tc.check([]byte(tc.body), tc.status); got != tc.want {
			t.Errorf("%s: challenge = %v, want %v", tc.name, got, tc.want)
		}
	}
}
