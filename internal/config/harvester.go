package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"time"
)

const HarvesterFileName = "harvester.config.json"
const DefaultHarvesterExternalPort = 18378

// HarvesterConfig is the fully materialized harvester.config.json. It is the
// ONLY source of Harvester settings: the harvest packages read no process
// environment, and the daemon units carry none.
type HarvesterConfig struct {
	External  HarvesterExternal
	Search    HarvesterSearch
	Scholarly HarvesterScholarly
	Fetch     HarvesterFetch
	Convert   HarvesterConvert
	Cache     HarvesterCache
	Output    HarvesterOutput
	Harvest   HarvesterLimits

	Path   string
	Exists bool
}

// HarvesterExternal is the authenticated gateway at its configured address.
type HarvesterExternal struct {
	Enabled     bool
	Host        string
	Port        int
	PublicURL   string
	Passphrase  string
	StaticToken string
	// StateDir holds the OAuth client/token store; empty resolves beside the
	// cache root at serve time.
	StateDir string
}

type HarvesterSearch struct {
	Enabled bool
	// SearXNGURL is operator configuration, so its exact origin is trusted by
	// the search request (a private/loopback SearXNG is the normal deployment).
	SearXNGURL  string
	BraveAPIKey string
}

type HarvesterScholarly struct {
	ContactEmail          string
	GoogleBooksAPIKey     string
	CoreAPIKey            string
	SemanticScholarAPIKey string
	DOIMirrorURL          string
	IPFSCatalogURL        string
	DOIViewerURL          string
	MD5CatalogURL         string
	GoogleScholarURL      string
}

type HarvesterFetch struct {
	Browser   bool
	UserAgent string
	ProxyURL  string
}

// HarvesterConvert steers the pinned Python converter. Go hands both flags to
// the worker process explicitly and sizes the worker pool from the rest.
type HarvesterConvert struct {
	// PDFOCR forces an OCR pass on every PDF (scanned/image-only documents).
	PDFOCR bool
	// PDFLayout keeps the PDF's physical layout in the extracted text.
	PDFLayout bool
	// Workers bounds the converter's parallel worker processes, each holding
	// the docling models (about 2 GB); 0 derives it from the CPUs and memory.
	Workers int
	// Queue bounds the conversions waiting for a worker before a new one is
	// refused as busy; 0 is eight per worker.
	Queue int
	// Timeout is one conversion's base deadline, scaled up by the input's size
	// and longest wait for a worker; 0 is 180 seconds.
	Timeout time.Duration
}

// HarvesterCache: an empty Dir resolves to harvest's single default,
// under the plugin state root. A zero TTL is meaningful: TTL 0 never expires a
// cached document; NegativeTTL / NegativeTransientTTL 0 never cache failures.
type HarvesterCache struct {
	Dir                  string
	TTL                  time.Duration
	NegativeTTL          time.Duration
	NegativeTransientTTL time.Duration
}

// HarvesterLimits caps what one retrieval may hold: MaxDownloadBytes one file
// download (harvest.maxDownloadBytes), MaxResourceBytes one blob the remote
// server sends through resources/read (harvest.maxResourceBytes). 0 = the
// harvester default (2 GiB, 25 MiB).
type HarvesterLimits struct {
	MaxDownloadBytes int64
	MaxResourceBytes int64
}

type HarvesterOutput struct {
	MaxInlineChars int
}

type rawHarvester struct {
	Ask       *rawAsk                `json:"ask,omitempty"`
	Claude    *rawBinary             `json:"claude,omitempty"`
	Codex     *rawBinary             `json:"codex,omitempty"`
	External  *rawHarvesterExternal  `json:"external,omitempty"`
	Search    *rawHarvesterSearch    `json:"search,omitempty"`
	Scholarly *rawHarvesterScholarly `json:"scholarly,omitempty"`
	Fetch     *rawHarvesterFetch     `json:"fetch,omitempty"`
	Convert   *rawHarvesterConvert   `json:"convert,omitempty"`
	Cache     *rawHarvesterCache     `json:"cache,omitempty"`
	Output    *rawHarvesterOutput    `json:"output,omitempty"`
	Harvest   *rawHarvesterLimits    `json:"harvest,omitempty"`
}

type rawHarvesterExternal struct {
	Enabled   *bool             `json:"enabled,omitempty"`
	Host      *string           `json:"host,omitempty"`
	Port      *int              `json:"port,omitempty"`
	PublicURL *string           `json:"publicURL,omitempty"`
	Auth      *rawHarvesterAuth `json:"auth,omitempty"`
	StateDir  *string           `json:"stateDir,omitempty"`
}

type rawHarvesterAuth struct {
	Passphrase  *string `json:"passphrase,omitempty"`
	StaticToken *string `json:"staticToken,omitempty"`
}

type rawHarvesterSearch struct {
	Enabled     *bool   `json:"enabled,omitempty"`
	SearXNGURL  *string `json:"searxngURL,omitempty"`
	BraveAPIKey *string `json:"braveApiKey,omitempty"`
}

type rawHarvesterScholarly struct {
	ContactEmail          *string           `json:"contactEmail,omitempty"`
	GoogleBooksAPIKey     *string           `json:"googleBooksApiKey,omitempty"`
	CoreAPIKey            *string           `json:"coreApiKey,omitempty"`
	SemanticScholarAPIKey *string           `json:"semanticScholarApiKey,omitempty"`
	GoogleScholarURL      *string           `json:"googleScholarURL,omitempty"`
	Mirrors               map[string]string `json:"mirrors,omitempty"`
}

type rawHarvesterFetch struct {
	Browser   *bool   `json:"browser,omitempty"`
	UserAgent *string `json:"userAgent,omitempty"`
	ProxyURL  *string `json:"proxyURL,omitempty"`
}

type rawHarvesterConvert struct {
	PDFOCR         *bool `json:"pdfOcr,omitempty"`
	PDFLayout      *bool `json:"pdfLayout,omitempty"`
	Workers        *int  `json:"workers,omitempty"`
	Queue          *int  `json:"queue,omitempty"`
	TimeoutSeconds *int  `json:"timeoutSeconds,omitempty"`
}

type rawHarvesterCache struct {
	Dir                         *string `json:"dir,omitempty"`
	TTLSeconds                  *int    `json:"ttlSeconds,omitempty"`
	NegativeTTLSeconds          *int    `json:"negativeTtlSeconds,omitempty"`
	NegativeTransientTTLSeconds *int    `json:"negativeTransientTtlSeconds,omitempty"`
}

type rawHarvesterLimits struct {
	MaxDownloadBytes *int64 `json:"maxDownloadBytes,omitempty"`
	MaxResourceBytes *int64 `json:"maxResourceBytes,omitempty"`
}

type rawHarvesterOutput struct {
	MaxInlineChars *int `json:"maxInlineChars,omitempty"`
}

// DefaultHarvester is the effective configuration when harvester.config.json
// is absent. MCP enablement belongs to the registering client.
func DefaultHarvester() HarvesterConfig {
	return HarvesterConfig{
		External: HarvesterExternal{Host: "127.0.0.1", Port: DefaultHarvesterExternalPort},
		Search:   HarvesterSearch{Enabled: true},
		Cache: HarvesterCache{
			TTL:                  24 * time.Hour,
			NegativeTTL:          120 * time.Second,
			NegativeTransientTTL: 15 * time.Second,
		},
		Output: HarvesterOutput{MaxInlineChars: 50000},
	}
}

func loadHarvester(result *Config, home string) error {
	harvester := result.Harvester
	harvester.Path = result.Path
	defer func() { result.Harvester = harvester }()
	info, statErr := os.Stat(harvester.Path)
	if errors.Is(statErr, fs.ErrNotExist) {
		return nil
	}
	if statErr != nil {
		return fmt.Errorf("inspect harvester config %s: %w", harvester.Path, statErr)
	}
	content, err := os.ReadFile(harvester.Path)
	if err != nil {
		return fmt.Errorf("read harvester config %s: %w", harvester.Path, err)
	}
	harvester.Exists = true
	var raw rawHarvester
	if err := decodeStrict(foldRetiredScholarlyKeys(content), &raw); err != nil {
		return configJSONError(harvester.Path, err, int64(len(content)))
	}
	path := harvester.Path
	if err := loadAsk(result, raw); err != nil {
		return fmt.Errorf("harvester config %s: %w", path, err)
	}
	if external := raw.External; external != nil {
		if external.Enabled != nil {
			harvester.External.Enabled = *external.Enabled
		}
		if external.Host != nil {
			host := strings.TrimSpace(*external.Host)
			if host == "" || strings.ContainsAny(host, " /\x00") {
				return fmt.Errorf(
					"harvester config %s: external.host must be a bare host or IP, got %q",
					path,
					*external.Host,
				)
			}
			harvester.External.Host = host
		}
		if external.Port != nil {
			if *external.Port < 1 || *external.Port > 65535 {
				return fmt.Errorf("harvester config %s: external.port must be between 1 and 65535", path)
			}
			harvester.External.Port = *external.Port
		}
		if external.PublicURL != nil {
			value := strings.TrimRight(strings.TrimSpace(*external.PublicURL), "/")
			if value != "" {
				if err := validateHTTPURL(value, false); err != nil {
					return fmt.Errorf("harvester config %s: external.publicURL %w", path, err)
				}
			}
			harvester.External.PublicURL = value
		}
		if external.Auth != nil {
			if external.Auth.Passphrase != nil {
				harvester.External.Passphrase = *external.Auth.Passphrase
			}
			if external.Auth.StaticToken != nil {
				harvester.External.StaticToken = strings.TrimSpace(*external.Auth.StaticToken)
			}
		}
		if external.StateDir != nil && strings.TrimSpace(*external.StateDir) != "" {
			dir, err := expandHomePath(strings.TrimSpace(*external.StateDir), home)
			if err != nil {
				return fmt.Errorf("harvester config %s: external.stateDir %w", path, err)
			}
			harvester.External.StateDir = dir
		}
	}
	if search := raw.Search; search != nil {
		if search.Enabled != nil {
			harvester.Search.Enabled = *search.Enabled
		}
		if search.SearXNGURL != nil {
			value := strings.TrimRight(strings.TrimSpace(*search.SearXNGURL), "/")
			if value != "" {
				if err := validateHTTPURL(value, true); err != nil {
					return fmt.Errorf("harvester config %s: search.searxngURL %w", path, err)
				}
			}
			harvester.Search.SearXNGURL = value
		}
		if search.BraveAPIKey != nil {
			harvester.Search.BraveAPIKey = strings.TrimSpace(*search.BraveAPIKey)
		}
	}
	if scholarly := raw.Scholarly; scholarly != nil {
		if scholarly.GoogleScholarURL != nil {
			value, err := scholarlyBaseURL(path, "googleScholarURL", *scholarly.GoogleScholarURL)
			if err != nil {
				return err
			}
			harvester.Scholarly.GoogleScholarURL = value
		}
		targets := map[string]*string{
			"doi-mirror":   &harvester.Scholarly.DOIMirrorURL,
			"doi-viewer":   &harvester.Scholarly.DOIViewerURL,
			"md5-catalog":  &harvester.Scholarly.MD5CatalogURL,
			"ipfs-catalog": &harvester.Scholarly.IPFSCatalogURL,
		}
		for id, raw := range scholarly.Mirrors {
			target, ok := targets[id]
			if !ok {
				return fmt.Errorf("harvester config %s: scholarly.mirrors: unknown provider %q", path, id)
			}
			value, err := scholarlyBaseURL(path, "mirrors."+id, raw)
			if err != nil {
				return err
			}
			*target = value
		}
		for _, pair := range map[string]struct {
			raw    *string
			target *string
		}{
			"contactEmail":          {scholarly.ContactEmail, &harvester.Scholarly.ContactEmail},
			"googleBooksApiKey":     {scholarly.GoogleBooksAPIKey, &harvester.Scholarly.GoogleBooksAPIKey},
			"coreApiKey":            {scholarly.CoreAPIKey, &harvester.Scholarly.CoreAPIKey},
			"semanticScholarApiKey": {scholarly.SemanticScholarAPIKey, &harvester.Scholarly.SemanticScholarAPIKey},
		} {
			if pair.raw != nil {
				*pair.target = strings.TrimSpace(*pair.raw)
			}
		}
	}
	if fetch := raw.Fetch; fetch != nil {
		if fetch.Browser != nil {
			harvester.Fetch.Browser = *fetch.Browser
		}
		if fetch.UserAgent != nil {
			if strings.ContainsAny(*fetch.UserAgent, "\r\n\x00") {
				return fmt.Errorf("harvester config %s: fetch.userAgent must be one line", path)
			}
			harvester.Fetch.UserAgent = strings.TrimSpace(*fetch.UserAgent)
		}
		if fetch.ProxyURL != nil {
			value := strings.TrimSpace(*fetch.ProxyURL)
			if value != "" {
				parsed, err := url.Parse(value)
				if err != nil || parsed.Scheme == "" || parsed.Host == "" {
					return fmt.Errorf(
						"harvester config %s: fetch.proxyURL must be an absolute proxy URL, got %q",
						path,
						value,
					)
				}
			}
			harvester.Fetch.ProxyURL = value
		}
	}
	if convert := raw.Convert; convert != nil {
		if convert.PDFOCR != nil {
			harvester.Convert.PDFOCR = *convert.PDFOCR
		}
		if convert.PDFLayout != nil {
			harvester.Convert.PDFLayout = *convert.PDFLayout
		}
		for _, pair := range []struct {
			key string
			raw *int
			set func(int)
		}{
			{"workers", convert.Workers, func(value int) { harvester.Convert.Workers = value }},
			{"queue", convert.Queue, func(value int) { harvester.Convert.Queue = value }},
			{"timeoutSeconds", convert.TimeoutSeconds, func(value int) {
				harvester.Convert.Timeout = time.Duration(value) * time.Second
			}},
		} {
			if pair.raw == nil {
				continue
			}
			if *pair.raw < 0 {
				return fmt.Errorf("harvester config %s: convert.%s must be 0 or more (0 = derived default), got %d",
					path, pair.key, *pair.raw)
			}
			pair.set(*pair.raw)
		}
	}
	if cache := raw.Cache; cache != nil {
		if cache.Dir != nil && strings.TrimSpace(*cache.Dir) != "" {
			dir, err := expandHomePath(strings.TrimSpace(*cache.Dir), home)
			if err != nil {
				return fmt.Errorf("harvester config %s: cache.dir %w", path, err)
			}
			harvester.Cache.Dir = dir
		}
		for key, pair := range map[string]struct {
			raw    *int
			target *time.Duration
		}{
			"ttlSeconds":                  {cache.TTLSeconds, &harvester.Cache.TTL},
			"negativeTtlSeconds":          {cache.NegativeTTLSeconds, &harvester.Cache.NegativeTTL},
			"negativeTransientTtlSeconds": {cache.NegativeTransientTTLSeconds, &harvester.Cache.NegativeTransientTTL},
		} {
			if pair.raw == nil {
				continue
			}
			if *pair.raw < 0 {
				return fmt.Errorf(
					"harvester config %s: cache.%s must be 0 or more (0 = never expire / never cache failures), got %d",
					path,
					key,
					*pair.raw,
				)
			}
			*pair.target = time.Duration(*pair.raw) * time.Second
		}
	}
	if raw.Output != nil && raw.Output.MaxInlineChars != nil {
		if *raw.Output.MaxInlineChars < 1 {
			return fmt.Errorf(
				"harvester config %s: output.maxInlineChars must be at least 1, got %d",
				path,
				*raw.Output.MaxInlineChars,
			)
		}
		harvester.Output.MaxInlineChars = *raw.Output.MaxInlineChars
	}
	if raw.Harvest != nil {
		for key, pair := range map[string]struct {
			raw    *int64
			target *int64
		}{
			"maxDownloadBytes": {raw.Harvest.MaxDownloadBytes, &harvester.Harvest.MaxDownloadBytes},
			"maxResourceBytes": {raw.Harvest.MaxResourceBytes, &harvester.Harvest.MaxResourceBytes},
		} {
			if pair.raw == nil {
				continue
			}
			if *pair.raw < 0 {
				return fmt.Errorf(
					"harvester config %s: harvest.%s must not be negative (0 = the default), got %d",
					path,
					key,
					*pair.raw,
				)
			}
			*pair.target = *pair.raw
		}
	}

	if harvester.External.Enabled {
		if harvester.External.PublicURL == "" {
			return fmt.Errorf(
				"harvester config %s: external.enabled requires external.publicURL (the URL clients reach the gateway at)",
				path,
			)
		}
		if harvester.External.Passphrase == "" && harvester.External.StaticToken == "" {
			return fmt.Errorf(
				"harvester config %s: external.enabled requires external.auth.passphrase and/or external.auth.staticToken — the external gateway is never unauthenticated",
				path,
			)
		}
	}
	if harvester.holdsSecret() && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf(
			"harvester config %s holds secrets but is readable by others (mode %04o); run: chmod 600 %s",
			path,
			info.Mode().Perm(),
			path,
		)
	}
	return nil
}

func (harvester HarvesterConfig) holdsSecret() bool {
	return harvester.External.Passphrase != "" || harvester.External.StaticToken != "" ||
		harvester.Search.BraveAPIKey != "" || harvester.Scholarly.GoogleBooksAPIKey != "" ||
		harvester.Scholarly.CoreAPIKey != "" || harvester.Scholarly.SemanticScholarAPIKey != ""
}

// validateHTTPURL accepts an absolute http(s) URL with a host and no
// userinfo. allowPath permits a sub-path mount (a SearXNG behind a prefix).
func validateHTTPURL(value string, allowPath bool) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("must be an absolute http(s) URL: %v", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("must use http or https, got %q", value)
	}
	if parsed.Hostname() == "" {
		return fmt.Errorf("must include a host, got %q", value)
	}
	if parsed.User != nil {
		return fmt.Errorf("must not carry userinfo, got %q", value)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("must not carry a query or fragment, got %q", value)
	}
	if !allowPath && parsed.Path != "" && parsed.Path != "/" {
		return fmt.Errorf("must be an origin without a path, got %q", value)
	}
	return nil
}

// MarshalHarvester renders the effective harvester config as the JSON shape
// of harvester.config.json. redact replaces every secret for display.
func MarshalHarvester(harvester HarvesterConfig, redact bool) ([]byte, error) {
	secret := func(value string) string {
		if redact && value != "" {
			return "<redacted>"
		}
		return value
	}
	value := map[string]any{
		"external": map[string]any{
			"enabled":   harvester.External.Enabled,
			"host":      harvester.External.Host,
			"port":      harvester.External.Port,
			"publicURL": harvester.External.PublicURL,
			"stateDir":  harvester.External.StateDir,
			"auth": map[string]any{
				"passphrase": secret(
					harvester.External.Passphrase,
				),
				"staticToken": secret(harvester.External.StaticToken),
			},
		},
		"search": map[string]any{
			"enabled": harvester.Search.Enabled, "searxngURL": harvester.Search.SearXNGURL,
			"braveApiKey": secret(harvester.Search.BraveAPIKey),
		},
		"scholarly": map[string]any{
			"contactEmail":          harvester.Scholarly.ContactEmail,
			"googleBooksApiKey":     secret(harvester.Scholarly.GoogleBooksAPIKey),
			"coreApiKey":            secret(harvester.Scholarly.CoreAPIKey),
			"semanticScholarApiKey": secret(harvester.Scholarly.SemanticScholarAPIKey),
			"googleScholarURL":      harvester.Scholarly.GoogleScholarURL,
		},
		"fetch": map[string]any{
			"browser": harvester.Fetch.Browser, "userAgent": harvester.Fetch.UserAgent,
			"proxyURL": harvester.Fetch.ProxyURL,
		},
		"convert": map[string]any{
			"pdfOcr": harvester.Convert.PDFOCR, "pdfLayout": harvester.Convert.PDFLayout,
			"workers": harvester.Convert.Workers, "queue": harvester.Convert.Queue,
			"timeoutSeconds": int(harvester.Convert.Timeout / time.Second),
		},
		"cache": map[string]any{
			"dir":                         harvester.Cache.Dir,
			"ttlSeconds":                  int(harvester.Cache.TTL / time.Second),
			"negativeTtlSeconds":          int(harvester.Cache.NegativeTTL / time.Second),
			"negativeTransientTtlSeconds": int(harvester.Cache.NegativeTransientTTL / time.Second),
		},
		"output": map[string]any{"maxInlineChars": harvester.Output.MaxInlineChars},
		"harvest": map[string]any{
			"maxDownloadBytes": harvester.Harvest.MaxDownloadBytes,
			"maxResourceBytes": harvester.Harvest.MaxResourceBytes,
		},
	}
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}

// scholarlyBaseURL trims and validates one scholarly provider base URL; an
// empty value disables that provider.
func scholarlyBaseURL(path, key, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if err := validateHTTPURL(value, true); err != nil {
		return "", fmt.Errorf("harvester config %s: scholarly.%s %w", path, key, err)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("harvester config %s: scholarly.%s: %w", path, key, err)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf(
			"harvester config %s: scholarly.%s must be a base URL without credentials, query, or fragment",
			path,
			key,
		)
	}
	return value, nil
}

// retiredScholarlyKeys maps the SHA-256 of each flat per-provider URL key an
// older harvester.config.json may still carry onto its scholarly.mirrors id.
var retiredScholarlyKeys = map[string]string{
	"d60bb4a7d2eb57e2b336ab25e532b8fc127df68256f8572b7f985b30496b9989": "doi-mirror",
	"005eaee92313cddb07b30ad9c3252ca9daa5296d84e89c291b2cc0130fa44d0c": "doi-viewer",
	"8819894e6f47fc084419444614f490cd6ab57da1a424f77e9c3fefcddbc7f380": "md5-catalog",
	"e176c422060328eab8a11936515dd1f5ccd86409eb76b0ceaa11fe7d61198368": "ipfs-catalog",
}

// foldRetiredScholarlyKeys rewrites retired flat provider keys into
// scholarly.mirrors so an older config keeps loading unchanged in effect. An
// explicit mirrors entry wins over a retired key for the same provider.
// Content that is not a JSON object, or carries no retired key, is returned
// untouched so decodeStrict reports its errors against the original bytes.
func foldRetiredScholarlyKeys(content []byte) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(content, &top); err != nil || top["scholarly"] == nil {
		return content
	}
	var scholarly map[string]json.RawMessage
	if err := json.Unmarshal(top["scholarly"], &scholarly); err != nil {
		return content
	}
	folded := map[string]json.RawMessage{}
	for key, value := range scholarly {
		sum := sha256.Sum256([]byte(key))
		if id, ok := retiredScholarlyKeys[hex.EncodeToString(sum[:])]; ok {
			folded[id] = value
			delete(scholarly, key)
		}
	}
	if len(folded) == 0 {
		return content
	}
	mirrors := map[string]json.RawMessage{}
	if raw, ok := scholarly["mirrors"]; ok {
		if err := json.Unmarshal(raw, &mirrors); err != nil {
			return content
		}
	}
	for id, value := range folded {
		if _, explicit := mirrors[id]; !explicit {
			mirrors[id] = value
		}
	}
	var err error
	if scholarly["mirrors"], err = json.Marshal(mirrors); err != nil {
		return content
	}
	if top["scholarly"], err = json.Marshal(scholarly); err != nil {
		return content
	}
	out, err := json.Marshal(top)
	if err != nil {
		return content
	}
	return out
}
