package harvestmcp

import (
	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/searxng"
)

// RuntimeFromConfig is harvester.config.json resolved into the MCP adapter's
// runtime — the ONE bridge between machine config and the harvester. Local
// callers (CLI, stdio, loopback daemon) keep the unconfined local-read surface
// subject to harvest.DenyLocalPath; the external gateway confines to exported artifacts.
// Its SearXNG origin is the operator's, else a ready managed backend's (searxng.Origin).
func RuntimeFromConfig(home string, harvester config.HarvesterConfig) Runtime {
	return Runtime{
		Home:                  home,
		CacheDir:              harvester.Cache.Dir,
		UserAgent:             harvester.Fetch.UserAgent,
		ProxyURL:              harvester.Fetch.ProxyURL,
		SearXNGURL:            searxng.Origin(home, harvester.Search),
		BraveAPIKey:           harvester.Search.BraveAPIKey,
		DisableSearch:         !harvester.Search.Enabled,
		ContactEmail:          harvester.Scholarly.ContactEmail,
		GoogleBooksAPIKey:     harvester.Scholarly.GoogleBooksAPIKey,
		CoreAPIKey:            harvester.Scholarly.CoreAPIKey,
		SemanticScholarAPIKey: harvester.Scholarly.SemanticScholarAPIKey,
		DOIMirrorURL:          harvester.Scholarly.DOIMirrorURL,
		IPFSCatalogURL:        harvester.Scholarly.IPFSCatalogURL,
		DOIViewerURL:          harvester.Scholarly.DOIViewerURL,
		MD5CatalogURL:         harvester.Scholarly.MD5CatalogURL,
		GoogleScholarURL:      harvester.Scholarly.GoogleScholarURL,
		Browser:               harvester.Fetch.Browser,
		PDFOCR:                harvester.Convert.PDFOCR,
		PDFLayout:             harvester.Convert.PDFLayout,
		ConvertWorkers:        harvester.Convert.Workers,
		ConvertQueue:          harvester.Convert.Queue,
		ConvertTimeout:        harvester.Convert.Timeout,
		CacheTTL:              harvester.Cache.TTL,
		NegativeTTL:           harvester.Cache.NegativeTTL,
		NegativeTransientTTL:  harvester.Cache.NegativeTransientTTL,
		TTLsConfigured:        true,
		MaxInlineChars:        harvester.Output.MaxInlineChars,
		MaxResultBytes:        harvester.Output.MaxResultBytes,
		MaxDownloadBytes:      harvester.Harvest.MaxDownloadBytes,
		MaxResourceBytes:      harvester.Harvest.MaxResourceBytes,
	}
}
