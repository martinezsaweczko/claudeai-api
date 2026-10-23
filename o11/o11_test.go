package o11

import (
	"context"
	"slices"
	"testing"

	"github.com/martinezsaweczko/whatsappBot-golang/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"
)

func TestNewPropagatorCarriesTraceContextAndBaggage(t *testing.T) {
	prop := newPropagator()

	fields := prop.Fields()
	for _, want := range []string{"traceparent", "baggage"} {
		if !slices.Contains(fields, want) {
			t.Errorf("propagator fields %v should include %q", fields, want)
		}
	}

	traceID, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	spanID, _ := trace.SpanIDFromHex("0102030405060708")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))
	member, err := baggage.NewMember("tenant", "kiosk")
	if err != nil {
		t.Fatalf("failed to build baggage member: %v", err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatalf("failed to build baggage: %v", err)
	}
	ctx = baggage.ContextWithBaggage(ctx, bag)

	carrier := propagation.MapCarrier{}
	prop.Inject(ctx, carrier)
	if got, want := carrier.Get("traceparent"), "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01"; got != want {
		t.Errorf("traceparent = %q, want %q", got, want)
	}

	extracted := prop.Extract(context.Background(), carrier)
	sc := trace.SpanContextFromContext(extracted)
	if sc.TraceID() != traceID || sc.SpanID() != spanID || !sc.IsRemote() {
		t.Errorf("extracted span context = %+v, want the injected remote one", sc)
	}
	if got := baggage.FromContext(extracted).Member("tenant").Value(); got != "kiosk" {
		t.Errorf("extracted baggage tenant = %q, want kiosk", got)
	}
}

// With neither a tracer endpoint nor a Prometheus path, telemetry must be
// inert: spans are never sampled and metrics go to a no-op provider. This is
// the only SetupOTelSDK configuration that needs no network and does not
// register a collector on the process-wide Prometheus registry.
func TestSetupOTelSDKWithoutExporters(t *testing.T) {
	prevTP, prevMP := otel.GetTracerProvider(), otel.GetMeterProvider()
	prevProp, prevLP := otel.GetTextMapPropagator(), logglobal.GetLoggerProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
		otel.SetTextMapPropagator(prevProp)
		logglobal.SetLoggerProvider(prevLP)
	})

	ctx := context.Background()
	shutdown, inst, err := SetupOTelSDK(ctx, &config.O11Config{Environment: "test"}, "whatsappbot-test", "v0.0.0")
	if err != nil {
		t.Fatalf("SetupOTelSDK failed: %v", err)
	}

	if inst.Trace == nil {
		t.Fatal("a tracer provider must always be returned")
	}
	_, span := inst.Trace.Tracer("test").Start(ctx, "inert")
	if span.IsRecording() || span.SpanContext().IsSampled() {
		t.Error("spans must not be recorded or sampled without a tracer endpoint")
	}
	span.End()

	if _, ok := inst.MeterProvider.(noop.MeterProvider); !ok {
		t.Errorf("meter provider is %T, want the no-op provider", inst.MeterProvider)
	}
	if !slices.Contains(otel.GetTextMapPropagator().Fields(), "traceparent") {
		t.Error("the global propagator should carry W3C trace context")
	}

	if err := shutdown(ctx); err != nil {
		t.Errorf("shutdown failed: %v", err)
	}
	// Each cleanup runs once: a second call has nothing left to do.
	if err := shutdown(ctx); err != nil {
		t.Errorf("second shutdown should be a no-op, got: %v", err)
	}
}

// resourceAttr returns the value of a resource attribute, or "" when absent.
func resourceAttr(res *resource.Resource, key string) string {
	for _, kv := range res.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.Emit()
		}
	}
	return ""
}

// The resource is what names the service in Tempo/Grafana: our own attributes
// must survive the merge with the SDK defaults and win over the environment.
func TestNewResourceIdentifiesService(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "no environment overrides"},
		{name: "OTEL_SERVICE_NAME set to another value", env: map[string]string{
			"OTEL_SERVICE_NAME": "http://tempo.monitoring.svc.cluster.local:3200",
		}},
		{name: "OTEL_RESOURCE_ATTRIBUTES set to other values", env: map[string]string{
			"OTEL_RESOURCE_ATTRIBUTES": "service.name=other,service.version=9.9.9,deployment.environment.name=dev",
		}},
	}

	want := map[string]string{
		"service.name":                "whatsappbot-golang",
		"service.version":             "1.2.3",
		"deployment.environment.name": "prod",
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Start from a known environment so the host cannot influence the result.
			// resource.Default() reads the environment only once per process, so
			// these cases pin the precedence whichever test triggers that read.
			t.Setenv("OTEL_SERVICE_NAME", "")
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			res := newResource("whatsappbot-golang", "1.2.3", "prod")

			for key, value := range want {
				if got := resourceAttr(res, key); got != value {
					t.Errorf("resource attribute %s = %q, want %q", key, got, value)
				}
			}
		})
	}
}
