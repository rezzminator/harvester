package setup

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rezzminator/harvester/internal/harvestmcp"
	"github.com/rezzminator/harvester/internal/paths"
)

func TestSetupMigratesDefaultOAuthClientsAndRefreshTokens(t *testing.T) {
	cfg, old, root := setupFixture(t)
	legacy := filepath.Join(cfg.Paths.Home, ".professor", ".harvester-state")
	const refresh = "fixture-persisted-refresh-token"
	state := fmt.Sprintf(`{"clients":[{"client_id":"persisted-client","token_endpoint_auth_method":"none","redirect_uris":["https://client.example/callback"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"scope":"harvest"}],"refresh":[{"_hash":"%x","client_id":"persisted-client","scopes":["harvest"]}]}`, sha256.Sum256([]byte(refresh)))
	putState(t, legacy, "auth.json", state)
	ops := fixtureOperations(t, old, root, false)
	if err := run(t.Context(), cfg, true, io.Discard, ops); err != nil {
		t.Fatal(err)
	}
	newState := filepath.Join(paths.StateRoot(cfg.Paths.Home), ".harvester-state", "auth.json")
	if raw, err := os.ReadFile(newState); err != nil || string(raw) != state {
		t.Fatalf("persisted OAuth state did not move intact: %s, %v", raw, err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy OAuth state remains: %v", err)
	}
	if err := run(t.Context(), cfg, true, io.Discard, ops); err != nil {
		t.Fatalf("idempotent setup: %v", err)
	}
	remote, err := harvestmcp.NewRemote(harvestmcp.RemoteOptions{
		Runtime:   harvestmcp.Runtime{CacheDir: paths.HarvesterCacheDir(cfg.Paths.Home)},
		PublicURL: "https://gateway.example", Passphrase: "fixture passphrase",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	body := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {"persisted-client"}}
	request := httptest.NewRequest(http.MethodPost, "https://gateway.example/token", strings.NewReader(body.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	remote.Handler().ServeHTTP(response, request)
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &tokens); err != nil || response.Code != http.StatusOK || tokens.AccessToken == "" || tokens.RefreshToken == "" {
		t.Fatalf("persisted client/refresh token rejected after setup: status=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
	request = httptest.NewRequest(http.MethodPost, "https://gateway.example/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	response = httptest.NewRecorder()
	remote.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"harvester"`) {
		t.Fatalf("renewed token failed MCP initialize: %d %s", response.Code, response.Body.String())
	}
}

func TestSetupPreservesExplicitOAuthStateLocations(t *testing.T) {
	for _, custom := range []string{"cache", "stateDir"} {
		t.Run(custom, func(t *testing.T) {
			cfg, old, root := setupFixture(t)
			legacy := filepath.Join(cfg.Paths.Home, ".professor", ".harvester-state")
			putState(t, legacy, "auth.json", "legacy state stays")
			if custom == "cache" {
				cfg.Config.Harvester.Cache.Dir = filepath.Join(cfg.Paths.Home, "custom", "cache")
				putState(t, filepath.Join(cfg.Paths.Home, "custom", ".harvester-state"), "auth.json", "custom cache state")
			} else {
				cfg.Config.Harvester.External.StateDir = filepath.Join(cfg.Paths.Home, "custom-auth")
				putState(t, cfg.Config.Harvester.External.StateDir, "auth.json", "custom external state")
			}
			if err := run(t.Context(), cfg, true, io.Discard, fixtureOperations(t, old, root, false)); err != nil {
				t.Fatal(err)
			}
			if raw, err := os.ReadFile(filepath.Join(legacy, "auth.json")); err != nil || string(raw) != "legacy state stays" {
				t.Fatalf("unselected legacy auth changed: %s %v", raw, err)
			}
			customState := cfg.Config.Harvester.External.StateDir
			if custom == "cache" {
				customState = filepath.Join(filepath.Dir(cfg.Config.Harvester.Cache.Dir), ".harvester-state")
			}
			if _, err := os.Stat(filepath.Join(customState, "auth.json")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(paths.StateRoot(cfg.Paths.Home), ".harvester-state")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unused default auth destination created: %v", err)
			}
		})
	}
}
