// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package opamp_test

import (
	"context"
	"log"

	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"

	"go.opentelemetry.io/contrib/otelconf/opamp"
)

// Example shows how to start a Manager that receives OpenTelemetry SDK
// configuration from an OpAMP server and installs it into the process globals.
func Example() {
	descr := &protobufs.AgentDescription{
		IdentifyingAttributes: []*protobufs.KeyValue{{
			Key:   "service.name",
			Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "my-service"}},
		}},
	}

	mgr, err := opamp.NewManager(
		opamp.WithServerURL("wss://localhost:4320/v1/opamp"),
		opamp.WithAgentDescription(descr),
		opamp.WithInstanceUID(types.InstanceUid{0x01, 0x02, 0x03}),
		opamp.WithStateStore(opamp.NewMemoryStateStore()),
		opamp.WithInstaller(opamp.GlobalInstaller{}),
	)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	if err := mgr.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := mgr.Shutdown(context.Background()); err != nil {
			log.Fatal(err)
		}
	}()

	// The manager now applies remote configuration as it arrives. Acquire
	// tracers, meters, and loggers after configuration is applied so they
	// observe the installed providers.
}

// ExampleNewSDK shows the high-level entry point: a single call constructs the
// manager, applies a bootstrap configuration, installs it into the OpenTelemetry
// globals, and starts the OpAMP client. Shutdown stops everything.
func ExampleNewSDK() {
	bootstrap := []byte(`
file_format: "0.3"
resource:
  attributes:
    - name: service.name
      value: my-service
`)

	sdk, err := opamp.NewSDK(context.Background(),
		opamp.WithServerURL("wss://localhost:4320/v1/opamp"),
		opamp.WithInstanceUID(types.InstanceUid{0x01, 0x02, 0x03}),
		opamp.WithBootstrap(bootstrap),
		opamp.WithInstaller(opamp.GlobalInstaller{}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := sdk.Shutdown(context.Background()); err != nil {
			log.Fatal(err)
		}
	}()

	// Providers are installed into the otel globals; acquire telemetry through
	// them so handles follow remote-config swaps.
}
