// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Command opampdemo connects to an OpAMP server, applies the OpenTelemetry SDK
// configuration it receives via go.opentelemetry.io/contrib/otelconf/opamp, and
// continuously emits a span so the effect of a newly applied configuration is
// visible on stdout.
//
// Run the reference OpAMP server from the opamp-go repository, then run this
// program and push a configuration through the server's web UI. See the package
// README in ../../ for step-by-step instructions.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"

	"go.opentelemetry.io/contrib/otelconf/opamp"
	"go.opentelemetry.io/otel"
)

// stdoutLogger is a types.Logger that writes to the standard logger.
type stdoutLogger struct{}

func (stdoutLogger) Debugf(_ context.Context, format string, v ...any) {
	log.Printf("[DEBUG] "+format, v...)
}

func (stdoutLogger) Errorf(_ context.Context, format string, v ...any) {
	log.Printf("[ERROR] "+format, v...)
}

func main() {
	serverURL := flag.String("server", "wss://127.0.0.1:4320/v1/opamp", "OpAMP server URL")
	serviceName := flag.String("service", "opampdemo", "service.name reported to the server")
	flag.Parse()

	descr := &protobufs.AgentDescription{
		IdentifyingAttributes: []*protobufs.KeyValue{{
			Key:   "service.name",
			Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: *serviceName}},
		}},
	}

	mgr, err := opamp.NewManager(
		opamp.WithServerURL(*serverURL),
		opamp.WithInstanceUID(types.InstanceUid{0xDE, 0xAD, 0xBE, 0xEF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}),
		opamp.WithAgentDescription(descr),
		opamp.WithLogger(stdoutLogger{}),
		opamp.WithInstaller(opamp.GlobalInstaller{}),
		// The reference opamp-go example server uses a self-signed certificate.
		opamp.WithTLSConfig(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // POC against a local example server.
	)
	if err != nil {
		log.Fatalf("create manager: %v", err)
	}

	ctx := context.Background()
	if err := mgr.Start(ctx); err != nil {
		log.Fatalf("start manager: %v", err)
	}
	defer func() {
		if err := mgr.Shutdown(context.Background()); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("connected to %s; emitting one span every 3s.", *serverURL)
	log.Printf("push an otelconf config with a console span exporter via the server UI to see spans printed below.")

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var n int
	for {
		select {
		case <-interrupt:
			log.Printf("shutting down...")
			return
		case <-ticker.C:
			// Acquire the tracer each tick so it observes the most recently
			// installed global TracerProvider.
			tr := otel.GetTracerProvider().Tracer("opampdemo")
			_, span := tr.Start(ctx, fmt.Sprintf("tick-%d", n))
			span.End()
			n++
		}
	}
}
