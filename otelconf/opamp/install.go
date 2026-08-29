// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package opamp // import "go.opentelemetry.io/contrib/otelconf/opamp"

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// SDK is a set of OpenTelemetry providers built from a configuration. It has
// been constructed but is not necessarily installed: installation is the
// responsibility of an [Installer]. Shutdown releases the resources held by the
// providers and must be safe to call exactly once.
type SDK struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	LoggerProvider log.LoggerProvider
	Propagator     propagation.TextMapPropagator
	Shutdown       func(context.Context) error
}

// shutdown calls the SDK's Shutdown func if both the SDK and the func are
// non-nil, returning nil otherwise. It keeps call sites free of nil checks.
func (s *SDK) shutdown(ctx context.Context) error {
	if s == nil || s.Shutdown == nil {
		return nil
	}
	return s.Shutdown(ctx)
}

// Installer makes a newly built SDK active. It is called with the next SDK and
// the previously installed SDK (nil on the first install). An Installer must not
// shut down previous: the [Manager] owns SDK lifecycle and shuts the previous
// SDK down only after Install returns successfully. Returning an error aborts
// the swap; the previous SDK remains active.
type Installer interface {
	Install(ctx context.Context, next SDK, previous *SDK) error
}

// GlobalInstaller installs an SDK by replacing the OpenTelemetry global
// providers and propagator. This is a process-wide side effect and only affects
// instrument and provider handles acquired after the swap.
type GlobalInstaller struct{}

// Install sets the global TracerProvider, MeterProvider, LoggerProvider, and
// TextMapPropagator to those of next. Nil fields are left unset.
func (GlobalInstaller) Install(_ context.Context, next SDK, _ *SDK) error {
	if next.TracerProvider != nil {
		otel.SetTracerProvider(next.TracerProvider)
	}
	if next.MeterProvider != nil {
		otel.SetMeterProvider(next.MeterProvider)
	}
	if next.LoggerProvider != nil {
		logglobal.SetLoggerProvider(next.LoggerProvider)
	}
	if next.Propagator != nil {
		otel.SetTextMapPropagator(next.Propagator)
	}
	return nil
}

// NoopInstaller performs no installation. It is useful for tests and for
// applications that want to receive built SDKs and install them themselves.
type NoopInstaller struct{}

// Install does nothing and never fails.
func (NoopInstaller) Install(context.Context, SDK, *SDK) error { return nil }
