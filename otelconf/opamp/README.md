# OpAMP-driven OpenTelemetry SDK Configuration

[![Go Reference](https://pkg.go.dev/badge/go.opentelemetry.io/contrib/otelconf/opamp.svg)](https://pkg.go.dev/go.opentelemetry.io/contrib/otelconf/opamp)

This module is a **private proof of concept**. It connects to an
[OpAMP](https://opentelemetry.io/docs/specs/opamp/) server, receives declarative
OpenTelemetry configuration, parses it with
[`otelconf`](https://pkg.go.dev/go.opentelemetry.io/contrib/otelconf), builds SDK
providers, and installs them — reporting apply status and effective configuration
back to the server.

```go
mgr, err := opamp.NewManager(
	opamp.WithServerURL("wss://localhost:4320/v1/opamp"),
	opamp.WithAgentDescription(descr),
	opamp.WithInstanceUID(uid),
	opamp.WithInstaller(opamp.GlobalInstaller{}),
)
if err != nil {
	return err
}
if err := mgr.Start(ctx); err != nil {
	return err
}
defer mgr.Shutdown(context.Background())
```

The API is unstable and not intended for upstream use. See the package
documentation for limitations.
