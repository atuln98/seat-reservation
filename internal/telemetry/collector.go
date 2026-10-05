package telemetry

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const maxCollectedTraces = 20000

type SpanRecord struct {
	SpanID   string
	ParentID string
	Name     string
	Start    time.Time
	Duration time.Duration
	Status   string
	Error    string
	Attrs    string
}

type Collector struct {
	mutex  sync.Mutex
	traces map[oteltrace.TraceID][]SpanRecord
}

var DefaultCollector = &Collector{traces: make(map[oteltrace.TraceID][]SpanRecord)}

func (collector *Collector) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (collector *Collector) OnEnd(span sdktrace.ReadOnlySpan) {
	parent := span.Parent()
	if !parent.IsValid() || parent.IsRemote() {
		return
	}
	record := SpanRecord{
		SpanID:   span.SpanContext().SpanID().String(),
		ParentID: parent.SpanID().String(),
		Name:     span.Name(),
		Start:    span.StartTime(),
		Duration: span.EndTime().Sub(span.StartTime()),
		Status:   statusName(span.Status().Code),
		Error:    span.Status().Description,
		Attrs:    formatAttributes(span.Attributes()),
	}
	traceID := span.SpanContext().TraceID()

	collector.mutex.Lock()
	defer collector.mutex.Unlock()
	spans, exists := collector.traces[traceID]
	if !exists && len(collector.traces) >= maxCollectedTraces {
		return
	}
	collector.traces[traceID] = append(spans, record)
}

func (collector *Collector) Shutdown(context.Context) error {
	return nil
}

func (collector *Collector) ForceFlush(context.Context) error {
	return nil
}

func (collector *Collector) Take(traceID oteltrace.TraceID) []SpanRecord {
	collector.mutex.Lock()
	spans := collector.traces[traceID]
	delete(collector.traces, traceID)
	collector.mutex.Unlock()
	sort.SliceStable(spans, func(first, second int) bool {
		return spans[first].Start.Before(spans[second].Start)
	})
	return spans
}

func statusName(code codes.Code) string {
	switch code {
	case codes.Error:
		return "error"
	case codes.Ok:
		return "ok"
	}
	return "unset"
}

func formatAttributes(attributes []attribute.KeyValue) string {
	var builder strings.Builder
	for _, item := range attributes {
		key := string(item.Key)
		if key == "db.system.name" || key == "db.operation.name" || strings.HasPrefix(key, "http.") || key == "url.path" || key == "request.id" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte(' ')
		}
		builder.WriteString(key)
		builder.WriteByte('=')
		value := item.Value.Emit()
		if len(value) > 120 {
			value = value[:120] + "..."
		}
		if strings.ContainsAny(value, " \"") {
			builder.WriteString(`"` + strings.ReplaceAll(value, `"`, `'`) + `"`)
		} else {
			builder.WriteString(value)
		}
	}
	return builder.String()
}
