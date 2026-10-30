package doctor

import (
	"context"
	"fmt"
	"github.com/rezzminator/harvester/internal/config"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

var RetiredHarvesterEnv = []struct{ Name, Now string }{
	{"SEARXNG_URL", "search.searxngURL"},
	{"BRAVE_API_KEY", "search.braveApiKey"},
	{"HARVESTER_DISABLE_SEARCH", "search.enabled"},
	{"HARVESTER_CONTACT_EMAIL", "scholarly.contactEmail"},
	{"GOOGLE_BOOKS_API_KEY", "scholarly.googleBooksApiKey"},
	{"CORE_API_KEY", "scholarly.coreApiKey"},
	{"SEMANTIC_SCHOLAR_API_KEY", "scholarly.semanticScholarApiKey"},
	{"HARVESTER_BROWSER", "fetch.browser"},
	{"HARVESTER_PDF_OCR", "convert.pdfOcr"},
	{"HARVESTER_PDF_LAYOUT", "convert.pdfLayout"},
	{"WEBFETCH_DIR", "cache.dir"},
	{"HARVESTER_CACHE_DIR", "cache.dir"},
	{"HARVESTER_CACHE_TTL", "cache.ttlSeconds"},
	{"HARVESTER_NEG_TTL", "cache.negativeTtlSeconds"},
	{"HARVESTER_NEG_TTL_TRANSIENT", "cache.negativeTransientTtlSeconds"},
	{"HARVESTER_MAX_INLINE_CHARS", "output.maxInlineChars"},
	{"HARVESTER_STATE_DIR", "external.stateDir"},
	{"HARVESTER_AUTH_PASSPHRASE", "external.auth.passphrase"},
	{"HARVESTER_STATIC_TOKEN", "external.auth.staticToken"},
	{"HARVESTER_LOCAL_ROOTS", ""},
}

func printHarvesterConfigDoctor(stdout io.Writer, runtime config.Runtime) int {
	warnings := 0
	for _, retired := range RetiredHarvesterEnv {
		if strings.TrimSpace(os.Getenv(retired.Name)) == "" {
			continue
		}
		warnings++
		if retired.Now == "" {
			fmt.Fprintf(stdout, "doctor: harvester retired_env=%s is set but ignored (removed; harvester never honored it)\n", retired.Name)
			continue
		}
		fmt.Fprintf(stdout, "doctor: harvester retired_env=%s is set but ignored — move it to %s in %s\n", retired.Name, retired.Now, runtime.Config.Path)
	}
	return warnings
}

var externalDial = (&net.Dialer{Timeout: 3 * time.Second}).DialContext

func printHarvesterExternalDoctor(ctx context.Context, stdout io.Writer, settings config.HarvesterConfig) int {
	if !settings.External.Enabled {
		return 0
	}
	host := settings.External.Host
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	address := net.JoinHostPort(host, strconv.Itoa(settings.External.Port))
	conn, err := externalDial(ctx, "tcp", address)
	if err != nil {
		fmt.Fprintf(stdout, "doctor: mcp harvester_external=not listening address=%s error=%v\n", address, err)
		return 1
	}
	if err := conn.Close(); err != nil {
		fmt.Fprintf(stdout, "doctor: mcp harvester_external=probe failed address=%s error=%v\n", address, err)
		return 1
	}
	fmt.Fprintf(stdout, "doctor: mcp harvester_external=listening address=%s\n", address)
	return 0
}
