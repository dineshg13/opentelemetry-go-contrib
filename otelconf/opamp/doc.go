// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Package opamp drives OpenTelemetry Go SDK configuration from an OpAMP server.
//
// A [Manager] connects to an OpAMP server, receives declarative OpenTelemetry
// configuration as remote config, parses it with
// go.opentelemetry.io/contrib/otelconf, builds fresh SDK providers, and installs
// them through an [Installer]. The previously installed SDK is shut down only
// after the replacement installs successfully. Configuration apply status and
// effective configuration are reported back to the server. The OpAMP protocol
// details (capabilities, status reporting, effective-config reporting) are owned
// by the manager; callers provide identity, state storage, and an install policy.
//
// [NewSDK] is the high-level entry point: it constructs a Manager, applies the
// startup configuration, and starts the OpAMP client in one call, so the SDK
// lifecycle (start on construction, stop on Shutdown) is handled for the caller.
// [NewManager] with an explicit [Manager.Start] is the lower-level API for
// callers that need finer control.
//
// # Startup configuration and authority
//
// At startup the manager applies a configuration and reports it to the server as
// the effective configuration on connect. The choice follows a resume-or-
// bootstrap policy: if the server has previously applied a configuration (a
// remote config status is persisted in the StateStore), that configuration is
// resumed so a restart while the server is unreachable does not regress to the
// bootstrap; otherwise the bootstrap configuration from [WithBootstrap] is used.
// After startup, configuration is server-authoritative: only the OpAMP server
// changes it. [Manager.ApplyConfig] exists for tests and non-server-driven local
// control, but the typical deployment relies solely on the server at runtime.
//
// # Agent identity
//
// Following the OpAMP OpenTelemetry SDK guidelines, the manager derives the
// AgentDescription from the same resource the SDK builds: service.name,
// service.instance.id, service.namespace, and telemetry.sdk.name are reported as
// identifying attributes and all other resource attributes (for example
// deployment.environment.name) as non-identifying. The
// description is refreshed whenever applied configuration changes the resource.
// A stable OpAMP instance UID is resolved from WithInstanceUID, the StateStore,
// or generated and persisted; the matching service.instance.id is injected into
// the resource when the config does not set one. Attributes supplied via
// [WithAgentDescription] are merged as non-identifying only and never override
// the derived service identity.
//
// This package is a private proof of concept. The API is unstable and not
// intended for upstream use.
//
// # Limitations
//
//   - Replacing the OpenTelemetry global providers does not affect Tracer, Meter,
//     or Logger handles that instrumentation has already cached. Runtime
//     reconfiguration is best effort and only fully takes effect for handles
//     acquired after the swap.
//   - Only remote declarative SDK configuration is handled. Packages, commands,
//     telemetry connection settings, and OpAMP connection settings are not.
//   - A remote config map with no empty ("") key and more than one entry is
//     rejected rather than merged.
//   - The AgentDescription is reconstructed from the parsed config (mirroring how
//     otelconf builds the resource: SDK defaults overlaid with config attributes);
//     resource.attributes_list and any future otelconf resource detectors are not
//     reflected, since otelconf does not expose the built resource.
package opamp // import "go.opentelemetry.io/contrib/otelconf/opamp"
