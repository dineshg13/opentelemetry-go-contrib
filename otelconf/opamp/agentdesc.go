// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package opamp // import "go.opentelemetry.io/contrib/otelconf/opamp"

import (
	"fmt"
	"sort"

	"github.com/open-telemetry/opamp-go/protobufs"

	"go.opentelemetry.io/contrib/otelconf"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Resource attribute keys that the OpAMP OpenTelemetry SDK guidelines require to
// be reported as identifying attributes. The spec writes service.namespace as
// "service.namespace.name"; both spellings are treated as identifying so the
// agent identity matches the telemetry resource regardless of convention.
const (
	keyServiceName          = "service.name"
	keyServiceInstanceID    = "service.instance.id"
	keyServiceNamespace     = "service.namespace"
	keyServiceNamespaceName = "service.namespace.name"
)

// identifyingKeys is the set of resource attribute keys that belong in
// AgentDescription.identifying_attributes per the OpAMP SDK guidelines.
var identifyingKeys = map[string]bool{
	keyServiceName:          true,
	keyServiceInstanceID:    true,
	keyServiceNamespace:     true,
	keyServiceNamespaceName: true,
}

// deriveAgentDescription builds an OpAMP AgentDescription from the SDK resource
// described by conf, following the OpAMP OpenTelemetry SDK guidelines:
//
//   - service.name, service.instance.id, and service.namespace are reported as
//     identifying attributes and MUST match the telemetry resource.
//   - all other resource attributes are reported as non-identifying attributes.
//
// The attribute set mirrors how otelconf builds the resource: the SDK default
// attributes (telemetry.sdk.*, service.name, and any from the environment)
// overlaid with the config's resource attributes. service.instance.id is forced
// to instanceID when the config does not set it, matching the value injected
// into the built resource. Caller-supplied attributes from extra are merged into
// the non-identifying set only; they never override derived service identity.
func deriveAgentDescription(conf *otelconf.OpenTelemetryConfiguration, instanceID string, extra *protobufs.AgentDescription) *protobufs.AgentDescription {
	attrs := map[string]*protobufs.AnyValue{}

	// SDK default attributes (mirrors otelconf.newResource using
	// resource.Default().Attributes()).
	for _, kv := range resource.Default().Attributes() {
		attrs[string(kv.Key)] = anyValue(kv.Value)
	}
	// Config-provided attributes take precedence over defaults.
	if conf != nil && conf.Resource != nil {
		for _, a := range conf.Resource.Attributes {
			attrs[a.Name] = anyValueFromAny(a.Value)
		}
	}
	// Guarantee a service.instance.id so the identity is complete.
	if _, ok := attrs[keyServiceInstanceID]; !ok && instanceID != "" {
		attrs[keyServiceInstanceID] = stringValue(instanceID)
	}

	var identifying, nonIdentifying []*protobufs.KeyValue
	for k, v := range attrs {
		kv := &protobufs.KeyValue{Key: k, Value: v}
		if identifyingKeys[k] {
			identifying = append(identifying, kv)
		} else {
			nonIdentifying = append(nonIdentifying, kv)
		}
	}

	// Merge caller-supplied attributes as non-identifying, skipping any key the
	// derived resource already defines (derived identity and attributes win).
	if extra != nil {
		merged := append([]*protobufs.KeyValue{}, extra.GetIdentifyingAttributes()...)
		merged = append(merged, extra.GetNonIdentifyingAttributes()...)
		for _, kv := range merged {
			if _, ok := attrs[kv.GetKey()]; ok {
				continue
			}
			attrs[kv.GetKey()] = kv.GetValue() // mark seen to dedupe across both lists
			nonIdentifying = append(nonIdentifying, kv)
		}
	}

	sortKeyValues(identifying)
	sortKeyValues(nonIdentifying)
	return &protobufs.AgentDescription{
		IdentifyingAttributes:    identifying,
		NonIdentifyingAttributes: nonIdentifying,
	}
}

// sortKeyValues orders attributes by key so the description is deterministic,
// which keeps change detection and tests stable across map iteration order.
func sortKeyValues(kvs []*protobufs.KeyValue) {
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].GetKey() < kvs[j].GetKey() })
}

// anyValue converts an OpenTelemetry attribute value to an OpAMP AnyValue.
func anyValue(v attribute.Value) *protobufs.AnyValue {
	switch v.Type() {
	case attribute.BOOL:
		return boolValue(v.AsBool())
	case attribute.INT64:
		return intValue(v.AsInt64())
	case attribute.FLOAT64:
		return doubleValue(v.AsFloat64())
	case attribute.STRING:
		return stringValue(v.AsString())
	case attribute.BOOLSLICE:
		s := v.AsBoolSlice()
		vals := make([]*protobufs.AnyValue, len(s))
		for i := range s {
			vals[i] = boolValue(s[i])
		}
		return arrayValue(vals)
	case attribute.INT64SLICE:
		s := v.AsInt64Slice()
		vals := make([]*protobufs.AnyValue, len(s))
		for i := range s {
			vals[i] = intValue(s[i])
		}
		return arrayValue(vals)
	case attribute.FLOAT64SLICE:
		s := v.AsFloat64Slice()
		vals := make([]*protobufs.AnyValue, len(s))
		for i := range s {
			vals[i] = doubleValue(s[i])
		}
		return arrayValue(vals)
	case attribute.STRINGSLICE:
		s := v.AsStringSlice()
		vals := make([]*protobufs.AnyValue, len(s))
		for i := range s {
			vals[i] = stringValue(s[i])
		}
		return arrayValue(vals)
	default:
		return stringValue(v.Emit())
	}
}

// anyValueFromAny converts a config attribute value (an untyped YAML scalar) to
// an OpAMP AnyValue, preserving native scalar types.
func anyValueFromAny(v any) *protobufs.AnyValue {
	switch val := v.(type) {
	case string:
		return stringValue(val)
	case bool:
		return boolValue(val)
	case int:
		return intValue(int64(val))
	case int64:
		return intValue(val)
	case float64:
		return doubleValue(val)
	default:
		return stringValue(fmt.Sprint(v))
	}
}

func stringValue(s string) *protobufs.AnyValue {
	return &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: s}}
}

func boolValue(b bool) *protobufs.AnyValue {
	return &protobufs.AnyValue{Value: &protobufs.AnyValue_BoolValue{BoolValue: b}}
}

func intValue(i int64) *protobufs.AnyValue {
	return &protobufs.AnyValue{Value: &protobufs.AnyValue_IntValue{IntValue: i}}
}

func doubleValue(f float64) *protobufs.AnyValue {
	return &protobufs.AnyValue{Value: &protobufs.AnyValue_DoubleValue{DoubleValue: f}}
}

func arrayValue(vals []*protobufs.AnyValue) *protobufs.AnyValue {
	return &protobufs.AnyValue{Value: &protobufs.AnyValue_ArrayValue{ArrayValue: &protobufs.ArrayValue{Values: vals}}}
}
