// Package otelres builds the OpenTelemetry resource shared by the log, trace
// and metric exporters, so all three describe the same service.
package otelres

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Build returns the default resource (SDK info, OTEL_RESOURCE_ATTRIBUTES,
// OTEL_SERVICE_NAME) extended with the non-empty values given.
func Build(serviceName, serviceVersion, environment string, extra map[string]string) (*resource.Resource, error) {
	var attrs []attribute.KeyValue
	add := func(k, v string) {
		if v != "" {
			attrs = append(attrs, attribute.String(k, v))
		}
	}
	add("service.name", serviceName)
	add("service.version", serviceVersion)
	add("deployment.environment", environment)
	for k, v := range extra {
		add(k, v)
	}
	return resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
}
