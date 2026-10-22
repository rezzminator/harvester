package doctor

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/rezzminator/harvester/internal/config"
)

func TestDoctorWarnsOnRetiredHarvesterEnv(t *testing.T) {
	for _, retired := range RetiredHarvesterEnv {
		t.Setenv(retired.Name, "")
	}
	t.Setenv("SEARXNG_URL", "http://127.0.0.1:8888")
	t.Setenv("HARVESTER_LOCAL_ROOTS", "/srv")
	var rows bytes.Buffer
	if warnings := printHarvesterConfigDoctor(&rows, config.Runtime{}); warnings != 2 {
		t.Fatalf("warnings=%d rows=%s", warnings, &rows)
	}
	for _, want := range []string{"retired_env=SEARXNG_URL", "search.searxngURL", "retired_env=HARVESTER_LOCAL_ROOTS", "never honored"} {
		if !strings.Contains(rows.String(), want) {
			t.Fatalf("missing %s in %s", want, &rows)
		}
	}
}

func TestDoctorExternalGatewayNeverRendersAsAbsence(t *testing.T) {
	previous := externalDial
	t.Cleanup(func() { externalDial = previous })
	settings := config.DefaultHarvester()
	settings.External.Enabled = true
	externalDial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("connection refused") }
	var rows bytes.Buffer
	if warnings := printHarvesterExternalDoctor(t.Context(), &rows, settings); warnings != 1 || !strings.Contains(rows.String(), "not listening") {
		t.Fatalf("warnings=%d rows=%s", warnings, &rows)
	}
	externalDial = func(context.Context, string, string) (net.Conn, error) {
		one, two := net.Pipe()
		two.Close()
		return one, nil
	}
	rows.Reset()
	if warnings := printHarvesterExternalDoctor(t.Context(), &rows, settings); warnings != 0 || !strings.Contains(rows.String(), "=listening") {
		t.Fatalf("warnings=%d rows=%s", warnings, &rows)
	}
}
