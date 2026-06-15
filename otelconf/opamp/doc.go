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
package opamp // import "go.opentelemetry.io/contrib/otelconf/opamp"
