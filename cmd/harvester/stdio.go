package main

import (
	"context"
	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/daemon"
	"io"
)

func serveStdio(ctx context.Context, runtime config.Runtime, input io.Reader, output, warnings io.Writer) error {
	return daemon.Proxy(ctx, runtime, input, output, warnings)
}
