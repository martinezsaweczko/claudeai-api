package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/martinezsaweczko/whatsappBot-golang/model"
	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// newTestInstrumentedDB wraps a temporary SQLite repository with no-op telemetry.
func newTestInstrumentedDB(t *testing.T) *InstrumentedDB {
	t.Helper()
	metrics, err := o11.NewRepoMetrics(noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("NewRepoMetrics failed: %v", err)
	}
	return NewInstrumented(newTestDB(t), metrics, tracenoop.NewTracerProvider())
}

func TestInstrumentedScheduledCommands(t *testing.T) {
	db := newTestInstrumentedDB(t)
	ctx := context.Background()

	id, err := db.CreateScheduledCommand(ctx, model.ScheduledCommand{
		Name: "morning", Schedule: "0 8 * * *", Command: "electricidad", GroupJID: "123@g.us", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateScheduledCommand failed: %v", err)
	}

	updated, err := db.UpdateScheduledCommand(ctx, model.ScheduledCommand{
		ID: id, Name: "evening", Schedule: "0 20 * * *", Command: "electricidad", GroupJID: "123@g.us", Enabled: false,
	})
	if err != nil {
		t.Fatalf("UpdateScheduledCommand failed: %v", err)
	}
	if updated.ID != id || updated.Name != "evening" || updated.UpdatedAt.IsZero() {
		t.Fatalf("wrapper did not return the stored row: %+v", updated)
	}

	got, err := db.GetScheduledCommand(ctx, id)
	if err != nil {
		t.Fatalf("GetScheduledCommand failed: %v", err)
	}
	if got.Name != "evening" || got.Schedule != "0 20 * * *" || got.Enabled {
		t.Fatalf("unexpected scheduled command after update: %+v", got)
	}

	cmds, err := db.ListScheduledCommands(ctx)
	if err != nil {
		t.Fatalf("ListScheduledCommands failed: %v", err)
	}
	if len(cmds) != 1 || cmds[0].ID != id {
		t.Fatalf("unexpected listing: %+v", cmds)
	}

	if err := db.DeleteScheduledCommand(ctx, id); err != nil {
		t.Fatalf("DeleteScheduledCommand failed: %v", err)
	}
}

// The wrappers must hand the not-found error through unchanged, because the
// layers above detect it with errors.Is.
func TestInstrumentedScheduledCommandsNotFound(t *testing.T) {
	db := newTestInstrumentedDB(t)
	ctx := context.Background()
	unknown := uuid.Must(uuid.NewV7())

	tests := []struct {
		name string
		call func() error
	}{
		{"get", func() error { _, err := db.GetScheduledCommand(ctx, unknown); return err }},
		{"update", func() error {
			_, err := db.UpdateScheduledCommand(ctx, model.ScheduledCommand{
				ID: unknown, Name: "ghost", Schedule: "0 8 * * *", Command: "electricidad", GroupJID: "1@g.us",
			})
			return err
		}},
		{"delete", func() error { return db.DeleteScheduledCommand(ctx, unknown) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("expected sql.ErrNoRows, got %v", err)
			}
		})
	}
}
