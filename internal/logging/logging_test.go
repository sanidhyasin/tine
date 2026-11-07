package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func logLine(t *testing.T, ctx context.Context, log func(*slog.Logger, context.Context)) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(NewHandler(slog.NewJSONHandler(&buf, nil)))
	log(logger, ctx)
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not one JSON line: %q", buf.String())
	}
	return line
}

func TestContextAttributesReachEveryLine(t *testing.T) {
	ctx := With(context.Background(), slog.String("request_id", "abc123"))
	ctx = WithVM(ctx, "vm-1")
	line := logLine(t, ctx, func(l *slog.Logger, ctx context.Context) { l.InfoContext(ctx, "hello", "n", 1) })
	if line["request_id"] != "abc123" || line["vm"] != "vm-1" || line["n"] != float64(1) || line["msg"] != "hello" {
		t.Errorf("line = %v", line)
	}
}

func TestLaterValuesReplaceEarlierOnes(t *testing.T) {
	ctx := WithVM(WithVM(context.Background(), "vm-old"), "vm-new")
	if attrs := Attrs(ctx); len(attrs) != 1 || attrs[0].Value.String() != "vm-new" {
		t.Errorf("attrs = %v, want just vm=vm-new", attrs)
	}
}

func TestARecordsOwnAttributeWins(t *testing.T) {
	ctx := WithVM(context.Background(), "vm-ctx")
	line := logLine(t, ctx, func(l *slog.Logger, ctx context.Context) { l.InfoContext(ctx, "x", "vm", "vm-explicit") })
	if line["vm"] != "vm-explicit" {
		t.Errorf("vm = %v, want the record's own value", line["vm"])
	}
}

func TestWithoutContextAttributesNothingChanges(t *testing.T) {
	line := logLine(t, context.Background(), func(l *slog.Logger, ctx context.Context) {
		l.With("component", "reaper").InfoContext(ctx, "tick")
	})
	if line["component"] != "reaper" || len(line) != 4 { // time, level, msg, component
		t.Errorf("line = %v", line)
	}
}

func TestRequestIDs(t *testing.T) {
	a, b := NewRequestID(), NewRequestID()
	if len(a) != 12 || a == b {
		t.Errorf("request IDs %q and %q: want 12 hex characters, different each time", a, b)
	}
}
