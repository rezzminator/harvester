package harvestpy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/clock"
	"github.com/rezzminator/harvester/internal/obs"
)

func shrinkDownloadRetryBackoff(t *testing.T) {
	t.Helper()
	was := downloadRetryBackoff
	downloadRetryBackoff = time.Millisecond
	t.Cleanup(func() { downloadRetryBackoff = was })
}

func TestDownloadFileRetriesServerFailure(t *testing.T) {
	shrinkDownloadRetryBackoff(t)
	ctx, recorder := obs.Test(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "try again", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "artifact")
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "artifact")
	if err := downloadFile(ctx, clock.Real, server.URL+"/archive?secret=hidden", path, 8); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "artifact" {
		t.Fatalf("body = %q, error = %v", body, err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	var retries, second int
	for _, record := range recorder.Records() {
		if record.Message == "harvestpy.download.retry" {
			retries++
			if record.Level != slog.LevelWarn.String() {
				t.Fatalf("retry level = %s", record.Level)
			}
			wantField(t, record, "path", "/archive")
		}
		if record.Message == "http.out.request" {
			second++
			if second == 2 {
				wantField(t, record, "retries", float64(1))
			}
		}
	}
	if retries != 1 || second != 2 || strings.Contains(recorder.Raw(), "hidden") {
		t.Fatalf("records: %s", recorder.Raw())
	}
}

// roundTripFunc answers a request without a network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (answer roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return answer(request)
}

// closeFailingBody is a complete response body whose Close errors.
type closeFailingBody struct{ io.Reader }

func (closeFailingBody) Close() error { return errors.New("connection reset by peer") }

// TestDownloadFileKeepsACompleteBodyWhoseCloseFails: every pinned byte arrived
// and the size matched, so a close error is logged, never the download's.
func TestDownloadFileKeepsACompleteBodyWhoseCloseFails(t *testing.T) {
	restore := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", ContentLength: 8, Request: request,
			Body: closeFailingBody{strings.NewReader("artifact")},
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = restore })
	ctx, recorder := obs.Test(t)
	path := filepath.Join(t.TempDir(), "artifact")
	if err := downloadFile(ctx, clock.Real, "https://archive.invalid/archive", path, 8); err != nil {
		t.Fatalf("download with a failing body close: %v", err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "artifact" {
		t.Fatalf("%s = %q, %v; want artifact", path, body, err)
	}
	var closes []string
	for _, record := range recorder.Records() {
		if record.Message == "harvestpy.download.close" {
			at, _ := record.Field("path")
			cause, _ := record.Field(obs.FieldErr)
			closes = append(closes, fmt.Sprintf("%s %v: %v", record.Level, at, cause))
		}
	}
	if want := []string{"WARN /archive: connection reset by peer"}; !slices.Equal(closes, want) {
		t.Fatalf("close records = %q, want %q", closes, want)
	}
}

func TestDownloadFileDoesNotRetry404(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.NotFound(w, nil)
	}))
	defer server.Close()
	ctx, recorder := obs.Test(t)
	err := downloadFile(ctx, clock.Real, server.URL, filepath.Join(t.TempDir(), "artifact"), 0)
	if err == nil || !strings.Contains(err.Error(), "download returned HTTP 404 Not Found") {
		t.Fatalf("error = %v", err)
	}
	if requests.Load() != 1 || strings.Contains(recorder.Raw(), "harvestpy.download.retry") {
		t.Fatalf("requests = %d; records = %s", requests.Load(), recorder.Raw())
	}
}

func TestDownloadFileGivesUpAfterServerFailures(t *testing.T) {
	shrinkDownloadRetryBackoff(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	err := downloadFile(context.Background(), clock.Real, server.URL, filepath.Join(t.TempDir(), "artifact"), 0)
	if err == nil ||
		!strings.Contains(err.Error(), "download returned HTTP 503 Service Unavailable") ||
		!strings.Contains(err.Error(), "after 4 attempts") {
		t.Fatalf("error = %v", err)
	}
	if got := requests.Load(); got != downloadAttempts {
		t.Fatalf("requests = %d, want %d", got, downloadAttempts)
	}
}

func TestDownloadFileRetriesDroppedConnection(t *testing.T) {
	shrinkDownloadRetryBackoff(t)
	ctx, recorder := obs.Test(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		_, _ = io.WriteString(w, "artifact")
	}))
	defer server.Close()
	if err := downloadFile(ctx, clock.Real, server.URL, filepath.Join(t.TempDir(), "artifact"), 8); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d", requests.Load())
	}
	for _, record := range recorder.Records() {
		if record.Message == "http.out.request" {
			if record.Level != slog.LevelWarn.String() {
				t.Fatalf("dropped attempt level = %s", record.Level)
			}
			return
		}
	}
	t.Fatal("no outbound record")
}

func TestDownloadFileDoesNotRetryChecksumMismatch(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, "wrong")
	}))
	defer server.Close()
	pinned := sha256.Sum256([]byte("right"))
	input := Artifact{URL: server.URL, Size: 5, SHA256: hex.EncodeToString(pinned[:])}
	err := ensureInput(context.Background(), filepath.Join(t.TempDir(), "artifact"), input, false, nil)
	if err == nil || !strings.Contains(err.Error(), "verify downloaded input") {
		t.Fatalf("error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d", requests.Load())
	}
}

func TestDownloadFileStopsWhenCancelledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	timing := cancelDownloadClock{Clock: clock.Real, cancel: cancel}
	err := downloadFile(ctx, timing, server.URL, filepath.Join(t.TempDir(), "artifact"), 0)
	if !errors.Is(err, context.Canceled) || requests.Load() != 1 {
		t.Fatalf("error = %v, requests = %d", err, requests.Load())
	}
}

type cancelDownloadClock struct {
	clock.Clock
	cancel context.CancelFunc
}

func (c cancelDownloadClock) Sleep(ctx context.Context, _ time.Duration) error {
	c.cancel()
	return ctx.Err()
}

func TestDownloadFileLastTransportFailureIsError(t *testing.T) {
	shrinkDownloadRetryBackoff(t)
	ctx, recorder := obs.Test(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}))
	defer server.Close()
	err := downloadFile(ctx, clock.Real, server.URL, filepath.Join(t.TempDir(), "artifact"), 0)
	if err == nil || !strings.Contains(err.Error(), "after 4 attempts") {
		t.Fatalf("error = %v", err)
	}
	var outbound []obs.Record
	for _, record := range recorder.Records() {
		if record.Message == "http.out.request" {
			outbound = append(outbound, record)
		}
	}
	if len(outbound) != downloadAttempts || outbound[len(outbound)-1].Level != slog.LevelError.String() {
		t.Fatalf("outbound records = %v", outbound)
	}
}
