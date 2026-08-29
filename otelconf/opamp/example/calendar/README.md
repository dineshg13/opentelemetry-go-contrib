# calendar (otelconf + OpAMP)

A small HTTP service that captures traces, metrics, and logs with OpenTelemetry,
configured **declaratively via [`otelconf`](https://pkg.go.dev/go.opentelemetry.io/contrib/otelconf)**
and driven by **OpAMP remote configuration** through
[`go.opentelemetry.io/contrib/otelconf/opamp`](../../).

Unlike the original calendar example, the SDK providers are not wired by hand.
Instead `opamp.NewSDK` parses a declarative config with `otelconf`, installs the
resulting providers into the OpenTelemetry globals, connects to an OpAMP server,
and live-swaps the SDK whenever the server pushes a new configuration.

```
config.yaml (bootstrap)            OpAMP server pushes config-datadog.yaml
        │                                       │
        ▼                                       ▼
otelconf.ParseYAML → otelconf.NewSDK → opamp.GlobalInstaller → otel globals
        ▲                                       │
        └─────────── opamp.NewSDK owns this lifecycle ───────┘
```

## Run it

The service starts with the embedded `config.yaml` (console exporters, so it
works with no collector running) and tries to connect to an OpAMP server in the
background. It serves immediately regardless of whether the server is reachable.

```bash
go run .                       # uses defaults below
curl localhost:9090/calendar   # → {"date":"2023-07-21","error_message":""}
```

You'll see JSON spans, metrics, and logs printed to stdout — produced by the
providers that `otelconf` built from `config.yaml`.

### Apply configuration live via OpAMP

1. Start the reference OpAMP server (web UI on `http://localhost:4321`):
   ```bash
   cd ~/otel/opamp-go/internal/examples && go run ./server
   ```
2. Run this service against it:
   ```bash
   OPAMP_ENDPOINT="wss://127.0.0.1:4320/v1/opamp" go run .
   ```
3. In the UI, open the `calendar-rest-go` agent and paste the contents of
   [`config-datadog.yaml`](./config-datadog.yaml) (OTLP/HTTP export, delta
   temporality). On **Save**, the service swaps from console to OTLP export with
   no restart, reports the new config back as its effective config, and persists
   it to the state directory. A broken YAML is reported as `FAILED` and the
   previous configuration keeps running.

## Environment variables

| Variable | Description | Default |
|----------|-------------|---------|
| `PORT` | HTTP server port | `9090` |
| `OTEL_SERVICE_NAME` | Service name reported to OpAMP | `calendar-rest-go` |
| `OTEL_CONFIG_FILE` | Path to a declarative config used as the bootstrap instead of the embedded `config.yaml` | embedded |
| `OPAMP_ENDPOINT` | OpAMP server URL (`ws(s)://…` or `http(s)://…`) | `ws://127.0.0.1:4320/v1/opamp` |
| `OPAMP_STATE_DIR` | Directory for persisted OpAMP state | `$TMPDIR/calendar-opamp` |

## Notes & limitations

- **Acquire telemetry through the otel globals.** The service builds instruments
  from `otel.GetMeterProvider()` / `otel.GetTracerProvider()` and logs through the
  global logger provider, so handles follow remote-config swaps. Code that caches
  a concrete provider at startup would not see swaps.
- **TLS:** the demo sets `InsecureSkipVerify` because the reference OpAMP server
  uses a self-signed certificate. Use a real `*tls.Config` (or the example CA) in
  production.
- **Docker/k8s:** the included `Dockerfile` and `k8s/` manifests are inherited
  from the original example. They will **not** build as-is, because this module
  depends on unpublished local contrib modules via `replace` directives, which a
  Docker build context cannot resolve. Vendor the dependencies (`go mod vendor`)
  or publish the modules before containerizing. This is a proof of concept.
