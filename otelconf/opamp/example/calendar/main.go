package main

import (
	"context"
	"crypto/tls"
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"go.opentelemetry.io/contrib/bridges/otelzap"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/contrib/otelconf/opamp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	defaultPort        = "9090"
	portStr            = "PORT"
	defaultServiceName = "calendar-rest-go"
	defaultOpAMPURL    = "ws://127.0.0.1:4320/v1/opamp"
)

// bootstrapConfig is the declarative OpenTelemetry configuration applied at
// startup before any remote configuration is received. It is parsed by otelconf.
//
//go:embed config.yaml
var bootstrapConfig []byte

var logger *zap.Logger

func main() {
	if err := realMain(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

// getEnv gets an environment variable or returns a default value if it is not set.
func getEnv(key, defaultValue string) string {
	value, ok := os.LookupEnv(key)
	if !ok {
		return defaultValue
	}
	return value
}

// zapOpAMPLogger adapts a *zap.Logger to the OpAMP client's types.Logger.
type zapOpAMPLogger struct{ l *zap.Logger }

func (z zapOpAMPLogger) Debugf(_ context.Context, format string, v ...any) {
	z.l.Sugar().Debugf(format, v...)
}

func (z zapOpAMPLogger) Errorf(_ context.Context, format string, v ...any) {
	z.l.Sugar().Errorf(format, v...)
}

func setupHandlers(server *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/calendar", otelhttp.NewHandler(http.HandlerFunc(server.calendarHandler), "CalendarHandler"))
	return mux
}

func realMain() error {
	ctx := context.Background()

	// Bootstrap logger used until the OpenTelemetry logger provider is installed.
	// Stack traces are limited to fatal errors so routine OpAMP reconnect
	// failures (which retry with exponential backoff) do not dump a trace each.
	var err error
	logger, err = zap.NewDevelopment(
		zap.AddStacktrace(zapcore.FatalLevel),
		zap.IncreaseLevel(zapcore.InfoLevel),
	)
	if err != nil {
		return err
	}

	serviceName := getEnv("OTEL_SERVICE_NAME", defaultServiceName)

	// The OpenTelemetry SDK is configured declaratively via otelconf. The
	// embedded config.yaml is the bootstrap; OTEL_CONFIG_FILE overrides it.
	cfg := bootstrapConfig
	if path := os.Getenv("OTEL_CONFIG_FILE"); path != "" {
		cfg, err = os.ReadFile(path) //nolint:gosec // operator-provided config path.
		if err != nil {
			return fmt.Errorf("read OTEL_CONFIG_FILE: %w", err)
		}
	}

	// Persist OpAMP state so that on restart the service reports its last
	// applied configuration instead of starting unconfigured.
	stateDir := getEnv("OPAMP_STATE_DIR", filepath.Join(os.TempDir(), "calendar-opamp"))
	store, err := opamp.NewFileStateStore(stateDir)
	if err != nil {
		return fmt.Errorf("create OpAMP state store: %w", err)
	}

	serverURL := getEnv("OPAMP_ENDPOINT", defaultOpAMPURL)

	// opamp.NewSDK builds providers from the bootstrap config, installs them into
	// the OpenTelemetry globals, connects to the OpAMP server, and keeps applying
	// remote configuration as it arrives. Shutdown stops the client and the
	// currently installed providers.
	//
	// The agent identity (service.name, service.instance.id) is derived from the
	// config's OpenTelemetry resource per the OpAMP SDK guidelines, and the OpAMP
	// instance UID is generated and persisted in the state store, so no explicit
	// AgentDescription or instance UID is needed here.
	sdk, err := opamp.NewSDK(ctx,
		opamp.WithServerURL(serverURL),
		opamp.WithBootstrap(cfg),
		opamp.WithStateStore(store),
		opamp.WithInstaller(opamp.GlobalInstaller{}),
		opamp.WithLogger(zapOpAMPLogger{logger}),
		// The reference opamp-go example server uses a self-signed certificate.
		opamp.WithTLSConfig(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // example against a local server.
	)
	if err != nil {
		return fmt.Errorf("initialize OpenTelemetry via OpAMP: %w", err)
	}
	defer func() {
		if err := sdk.Shutdown(context.Background()); err != nil {
			logger.Error("opamp SDK shutdown failed", zap.Error(err))
		}
	}()

	// Route application logs through the installed (global) logger provider so
	// they follow any remote-config swap.
	logger = zap.New(otelzap.NewCore(serviceName, otelzap.WithLoggerProvider(global.GetLoggerProvider())))
	logger.Info("OpenTelemetry configured via otelconf + OpAMP",
		zap.String("service.name", serviceName),
		zap.String("opamp.endpoint", serverURL),
		zap.String("opamp.state_dir", stateDir),
	)

	// Build the server from the global providers. Instruments created from the
	// global delegating providers follow remote-config swaps.
	server, err := NewServer(serviceName, otel.GetMeterProvider(), otel.GetTracerProvider())
	if err != nil {
		logger.Error("can't create new server", zap.Error(err))
		return err
	}

	endpoint := fmt.Sprintf(":%s", getEnv(portStr, defaultPort))
	lis, err := net.Listen("tcp", endpoint)
	if err != nil {
		return err
	}
	mux := setupHandlers(server)
	logger.Info("Starting server", zap.String("endpoint", endpoint))
	if err := http.Serve(lis, mux); err != nil { //nolint:gosec // example server.
		logger.Error("http server error", zap.Error(err))
		return err
	}
	return nil
}
