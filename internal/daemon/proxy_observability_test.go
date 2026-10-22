package daemon

import (
	"bytes"
	"github.com/rezzminator/harvester/internal/obs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOptionalProxyRequestsRecordWarn(t *testing.T) {
	// Its subtests replace the process logger through obs.Test.
	server := httptest.NewServer(http.NotFoundHandler())
	address := strings.TrimPrefix(server.URL, "http://")
	server.Close()
	assertOwnWarn := func(t *testing.T, recorder *obs.Recorder) {
		t.Helper()
		var own []obs.Record
		for _, record := range recorder.Records() {
			if host, ok := record.Field("host"); ok && host == address {
				own = append(own, record)
			}
		}
		if len(own) != 1 || own[0].Level != "WARN" {
			t.Fatalf("own-host records=%v; all records=%s", own, recorder.Raw())
		}
	}
	t.Run("close", func(t *testing.T) {
		_, recorder := obs.Test(t)
		var warnings bytes.Buffer
		proxy := &stdioProxy{
			endpoint: server.URL, client: obs.WrapClient(&http.Client{}),
			warnings: &warnings, sessionID: "session",
		}
		proxy.closeSession()
		if !strings.Contains(warnings.String(), "close daemon session") {
			t.Fatalf("warning=%s", warnings.String())
		}
		assertOwnWarn(t, recorder)
	})
}
