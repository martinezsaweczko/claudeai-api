package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/martinezsaweczko/whatsappBot-golang/model"
	"github.com/robfig/cron/v3"
	"go.mau.fi/whatsmeow/types"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// fakeUpdatedAt is the timestamp the fake store assigns on update, standing in
// for the one the database sets.
var fakeUpdatedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// fakeStore is an in-memory scheduler store for tests. It is safe for
// concurrent use and can be told to fail or to record calls.
type fakeStore struct {
	mu          sync.Mutex
	cmds        []model.ScheduledCommand
	listErr     error
	updateErr   error
	updateCalls int
}

func (f *fakeStore) CreateScheduledCommand(ctx context.Context, cmd model.ScheduledCommand) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cmd.ID == uuid.Nil {
		cmd.ID = uuid.Must(uuid.NewV7())
	}
	f.cmds = append(f.cmds, cmd)
	return cmd.ID, nil
}

func (f *fakeStore) ListScheduledCommands(ctx context.Context) ([]model.ScheduledCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]model.ScheduledCommand(nil), f.cmds...), nil
}

func (f *fakeStore) GetScheduledCommand(ctx context.Context, id uuid.UUID) (model.ScheduledCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if c.ID == id {
			return c, nil
		}
	}
	return model.ScheduledCommand{}, sql.ErrNoRows
}

func (f *fakeStore) UpdateScheduledCommand(ctx context.Context, cmd model.ScheduledCommand) (model.ScheduledCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	if f.updateErr != nil {
		return model.ScheduledCommand{}, f.updateErr
	}
	for i, c := range f.cmds {
		if c.ID == cmd.ID {
			cmd.CreatedAt = c.CreatedAt
			cmd.UpdatedAt = fakeUpdatedAt
			f.cmds[i] = cmd
			return cmd, nil
		}
	}
	return model.ScheduledCommand{}, sql.ErrNoRows
}

func (f *fakeStore) DeleteScheduledCommand(ctx context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.cmds {
		if c.ID == id {
			f.cmds = append(f.cmds[:i], f.cmds[i+1:]...)
			return nil
		}
	}
	return sql.ErrNoRows
}

// fakeExecutor matches commands and records executions.
type fakeExecutor struct {
	commands []string
	execs    []struct {
		Body string
		Chat types.JID
	}
}

func (f *fakeExecutor) ExecuteCommand(ctx context.Context, body string, chat, senderJID types.JID) error {
	if _, err := f.Match(body); err != nil {
		return err
	}
	f.execs = append(f.execs, struct {
		Body string
		Chat types.JID
	}{Body: body, Chat: chat})
	return nil
}

func (f *fakeExecutor) Match(body string) (string, error) {
	for _, cmd := range f.commands {
		if body == cmd {
			return cmd, nil
		}
	}
	return "", errors.New("no match")
}

// fakeChecker reports connected and a fixed own JID.
type fakeChecker struct {
	connected bool
	ownJID    types.JID
}

func (f *fakeChecker) IsConnected() bool { return f.connected }
func (f *fakeChecker) OwnJID() types.JID { return f.ownJID }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestCreateValidatesCron(t *testing.T) {
	svc, err := New(&fakeStore{}, &fakeExecutor{}, &fakeChecker{}, testLogger(), noop.NewTracerProvider(), Config{Timezone: "Europe/Madrid"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	_, err = svc.Create(context.Background(), model.ScheduledCommand{
		Name:     "bad",
		Schedule: "not a cron",
		Command:  "x",
		GroupJID: "123@g.us",
	})
	if err == nil {
		t.Fatal("expected error for invalid cron expression")
	}
}

func TestCreateValidatesCommand(t *testing.T) {
	svc, err := New(&fakeStore{}, &fakeExecutor{commands: []string{"electricidad"}}, &fakeChecker{}, testLogger(), noop.NewTracerProvider(), Config{Timezone: "Europe/Madrid"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	_, err = svc.Create(context.Background(), model.ScheduledCommand{
		Name:     "bad",
		Schedule: "0 8 * * *",
		Command:  "unknown",
		GroupJID: "123@g.us",
	})
	if err == nil {
		t.Fatal("expected error for unknown command")
	}
}

func TestCreateValidatesGroupJID(t *testing.T) {
	svc, err := New(&fakeStore{}, &fakeExecutor{commands: []string{"electricidad"}}, &fakeChecker{}, testLogger(), noop.NewTracerProvider(), Config{Timezone: "Europe/Madrid"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	_, err = svc.Create(context.Background(), model.ScheduledCommand{
		Name:     "bad",
		Schedule: "0 8 * * *",
		Command:  "electricidad",
		GroupJID: "123@c.us",
	})
	if err == nil {
		t.Fatal("expected error for non-group JID")
	}
}

func TestCreateStoresAndRegistersCronJob(t *testing.T) {
	store := &fakeStore{}
	exec := &fakeExecutor{commands: []string{"electricidad"}}
	checker := &fakeChecker{connected: true, ownJID: types.NewJID("bot", types.DefaultUserServer)}
	svc, err := New(store, exec, checker, testLogger(), noop.NewTracerProvider(), Config{Timezone: "Europe/Madrid"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer svc.Stop()

	id, err := svc.Create(context.Background(), model.ScheduledCommand{
		Name:     "morning",
		Schedule: "0 8 * * *",
		Command:  "electricidad",
		GroupJID: "123456789@g.us",
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("expected non-nil id")
	}

	if _, ok := svc.entryFor(id); !ok {
		t.Fatal("expected cron entry to be registered")
	}
}

func TestRunJobExecutesWhenConnected(t *testing.T) {
	exec := &fakeExecutor{commands: []string{"electricidad"}}
	checker := &fakeChecker{connected: true, ownJID: types.NewJID("bot", types.DefaultUserServer)}
	svc, err := New(&fakeStore{}, exec, checker, testLogger(), noop.NewTracerProvider(), Config{Timezone: "Europe/Madrid"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	id := uuid.Must(uuid.NewV7())
	svc.runJob(id, "0 8 * * *", "electricidad", "123456789@g.us")

	if len(exec.execs) != 1 {
		t.Fatalf("expected 1 execution, got %d", len(exec.execs))
	}
	if exec.execs[0].Body != "electricidad" {
		t.Fatalf("unexpected command body: %s", exec.execs[0].Body)
	}
	if exec.execs[0].Chat.String() != "123456789@g.us" {
		t.Fatalf("unexpected chat: %s", exec.execs[0].Chat.String())
	}
}

// panickingExecutor panics on every execution, like a buggy command handler.
type panickingExecutor struct{ fakeExecutor }

func (p *panickingExecutor) ExecuteCommand(ctx context.Context, body string, chat, senderJID types.JID) error {
	panic("handler exploded")
}

func TestRunJobRecoversPanic(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	exec := &panickingExecutor{fakeExecutor{commands: []string{"electricidad"}}}
	checker := &fakeChecker{connected: true, ownJID: types.NewJID("bot", types.DefaultUserServer)}
	svc, err := New(&fakeStore{}, exec, checker, testLogger(), tp, Config{Timezone: "Europe/Madrid"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Must return normally: an unrecovered panic here would crash the process
	svc.runJob(uuid.Must(uuid.NewV7()), "0 8 * * *", "electricidad", "123456789@g.us")

	span := singleSpan(t, recorder, "scheduler.RunJob")
	if span.Status().Code != codes.Error {
		t.Errorf("span status = %v, want %v", span.Status().Code, codes.Error)
	}
	if !strings.Contains(span.Status().Description, "handler exploded") {
		t.Errorf("span status description = %q, want the panic value", span.Status().Description)
	}
	if !hasEvent(span, "exception") {
		t.Error("expected the panic to be recorded as an exception event")
	}
}

func TestDeleteRemovesJob(t *testing.T) {
	store := &fakeStore{}
	exec := &fakeExecutor{commands: []string{"electricidad"}}
	checker := &fakeChecker{connected: true, ownJID: types.NewJID("bot", types.DefaultUserServer)}
	svc, err := New(store, exec, checker, testLogger(), noop.NewTracerProvider(), Config{Timezone: "Europe/Madrid"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer svc.Stop()

	id, err := svc.Create(context.Background(), model.ScheduledCommand{
		Name:     "morning",
		Schedule: "0 8 * * *",
		Command:  "electricidad",
		GroupJID: "123456789@g.us",
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if _, ok := svc.entryFor(id); !ok {
		t.Fatal("expected cron entry to be registered before delete")
	}

	if err := svc.Delete(context.Background(), id); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if _, ok := svc.entryFor(id); ok {
		t.Fatal("expected cron entry to be removed after delete")
	}
}

func TestRunJobSkipsWhenDisconnected(t *testing.T) {
	exec := &fakeExecutor{commands: []string{"electricidad"}}
	checker := &fakeChecker{connected: false}
	svc, err := New(&fakeStore{}, exec, checker, testLogger(), noop.NewTracerProvider(), Config{Timezone: "Europe/Madrid"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	id := uuid.Must(uuid.NewV7())
	svc.runJob(id, "* * * * *", "electricidad", "123456789@g.us")

	if len(exec.execs) != 0 {
		t.Fatal("expected no execution when disconnected")
	}
}

func TestInvalidTimezone(t *testing.T) {
	_, err := New(&fakeStore{}, &fakeExecutor{}, &fakeChecker{}, testLogger(), noop.NewTracerProvider(), Config{Timezone: "Mars/Olympus"})
	if err == nil {
		t.Fatal("expected error for invalid timezone")
	}
}

func TestParseCron(t *testing.T) {
	if _, err := cron.ParseStandard("0 8 * * *"); err != nil {
		t.Fatalf("standard cron parse failed: %v", err)
	}
}

// startedService returns a running scheduler with one enabled command stored
// and registered, plus the cron entry ID that command got.
func startedService(t *testing.T) (*Service, *fakeStore, uuid.UUID, cron.EntryID) {
	t.Helper()
	store := &fakeStore{}
	exec := &fakeExecutor{commands: []string{"electricidad", "tiempo"}}
	checker := &fakeChecker{connected: true, ownJID: types.NewJID("bot", types.DefaultUserServer)}
	svc, err := New(store, exec, checker, testLogger(), noop.NewTracerProvider(), Config{Timezone: "Europe/Madrid"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(svc.Stop)

	id, err := svc.Create(context.Background(), model.ScheduledCommand{
		Name:     "morning",
		Schedule: "0 8 * * *",
		Command:  "electricidad",
		GroupJID: "123456789@g.us",
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	entryID, ok := svc.entryFor(id)
	if !ok {
		t.Fatal("expected cron entry to be registered after create")
	}
	return svc, store, id, entryID
}

// entryFor reads the cron entry of a scheduled command under the service lock.
func (s *Service) entryFor(id uuid.UUID) (cron.EntryID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entryID, ok := s.entries[id]
	return entryID, ok
}

func TestUpdateReplacesCronEntry(t *testing.T) {
	svc, store, id, oldEntry := startedService(t)

	got, err := svc.Update(context.Background(), model.ScheduledCommand{
		ID:       id,
		Name:     "evening",
		Schedule: "30 20 * * *",
		Command:  "tiempo",
		GroupJID: "987654321@g.us",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	if got.ID != id || got.Name != "evening" || got.Schedule != "30 20 * * *" || got.Command != "tiempo" ||
		got.GroupJID != "987654321@g.us" || !got.Enabled {
		t.Fatalf("unexpected returned command: %+v", got)
	}
	if !got.UpdatedAt.Equal(fakeUpdatedAt) {
		t.Fatalf("expected the timestamp assigned by the store, got %v", got.UpdatedAt)
	}
	if store.updateCalls != 1 {
		t.Fatalf("expected exactly 1 store update, got %d", store.updateCalls)
	}
	if stored, _ := store.GetScheduledCommand(context.Background(), id); stored.Schedule != "30 20 * * *" {
		t.Fatalf("store was not updated: %+v", stored)
	}

	newEntry, ok := svc.entryFor(id)
	if !ok {
		t.Fatal("expected a cron entry after update")
	}
	if newEntry == oldEntry {
		t.Fatal("expected the cron entry to be replaced")
	}
	if svc.cron.Entry(oldEntry).Valid() {
		t.Fatal("expected the old cron entry to be removed from the runner")
	}
	if !svc.cron.Entry(newEntry).Valid() {
		t.Fatal("expected the new cron entry to be registered in the runner")
	}
	if n := len(svc.cron.Entries()); n != 1 {
		t.Fatalf("expected exactly 1 cron entry, got %d", n)
	}
}

func TestUpdateDisabledRemovesCronEntry(t *testing.T) {
	svc, _, id, oldEntry := startedService(t)

	got, err := svc.Update(context.Background(), model.ScheduledCommand{
		ID: id, Name: "morning", Schedule: "0 8 * * *", Command: "electricidad", GroupJID: "123456789@g.us", Enabled: false,
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if got.Enabled {
		t.Fatal("expected the returned command to be disabled")
	}
	if _, ok := svc.entryFor(id); ok {
		t.Fatal("expected no cron entry for a disabled command")
	}
	if svc.cron.Entry(oldEntry).Valid() {
		t.Fatal("expected the cron entry to be removed from the runner")
	}

	// Enabling it again brings the job back.
	if _, err := svc.Update(context.Background(), model.ScheduledCommand{
		ID: id, Name: "morning", Schedule: "0 8 * * *", Command: "electricidad", GroupJID: "123456789@g.us", Enabled: true,
	}); err != nil {
		t.Fatalf("re-enabling Update failed: %v", err)
	}
	if _, ok := svc.entryFor(id); !ok {
		t.Fatal("expected a cron entry after re-enabling")
	}
}

func TestUpdateValidation(t *testing.T) {
	valid := model.ScheduledCommand{
		Name: "morning", Schedule: "0 8 * * *", Command: "electricidad", GroupJID: "123456789@g.us", Enabled: true,
	}
	tests := []struct {
		name    string
		mutate  func(*model.ScheduledCommand)
		wantMsg string
	}{
		{"missing name", func(c *model.ScheduledCommand) { c.Name = "" }, "name is required"},
		{"missing schedule", func(c *model.ScheduledCommand) { c.Schedule = "" }, "schedule is required"},
		{"invalid cron", func(c *model.ScheduledCommand) { c.Schedule = "not a cron" }, "invalid cron expression"},
		{"missing command", func(c *model.ScheduledCommand) { c.Command = "" }, "command is required"},
		{"unknown command", func(c *model.ScheduledCommand) { c.Command = "unknown" }, "command does not match any registered handler"},
		{"missing group", func(c *model.ScheduledCommand) { c.GroupJID = "" }, "group_jid is required"},
		{"non-group JID", func(c *model.ScheduledCommand) { c.GroupJID = "123@c.us" }, "invalid group JID: 123@c.us"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, store, id, oldEntry := startedService(t)
			cmd := valid
			cmd.ID = id
			tt.mutate(&cmd)

			_, err := svc.Update(context.Background(), cmd)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("expected ErrInvalid, got %v", err)
			}
			if !strings.HasPrefix(err.Error(), tt.wantMsg) {
				t.Fatalf("expected message starting with %q, got %q", tt.wantMsg, err.Error())
			}
			if store.updateCalls != 0 {
				t.Fatalf("expected the store not to be called, got %d calls", store.updateCalls)
			}
			if entry, _ := svc.entryFor(id); entry != oldEntry {
				t.Fatal("expected the cron entry to be untouched")
			}
		})
	}
}

func TestCreateValidationMatchesSentinel(t *testing.T) {
	svc, err := New(&fakeStore{}, &fakeExecutor{}, &fakeChecker{}, testLogger(), noop.NewTracerProvider(), Config{Timezone: "Europe/Madrid"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	_, err = svc.Create(context.Background(), model.ScheduledCommand{Schedule: "0 8 * * *"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
	if err.Error() != "name is required" {
		t.Fatalf("validation message changed: %q", err.Error())
	}
}

func TestUpdateStoreFailureKeepsCronEntry(t *testing.T) {
	svc, store, id, oldEntry := startedService(t)
	storeErr := errors.New("database is down")
	store.updateErr = storeErr

	got, err := svc.Update(context.Background(), model.ScheduledCommand{
		ID: id, Name: "evening", Schedule: "30 20 * * *", Command: "tiempo", GroupJID: "987654321@g.us", Enabled: true,
	})
	if !errors.Is(err, storeErr) {
		t.Fatalf("expected the store error, got %v", err)
	}
	if got != (model.ScheduledCommand{}) {
		t.Fatalf("expected a zero command on failure, got %+v", got)
	}
	if errors.Is(err, ErrInvalid) || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("store failure must not look like a validation or not-found error: %v", err)
	}
	entry, ok := svc.entryFor(id)
	if !ok || entry != oldEntry {
		t.Fatal("expected the original cron entry to stay registered")
	}
	if !svc.cron.Entry(oldEntry).Valid() {
		t.Fatal("expected the original cron entry to stay in the runner")
	}
	if n := len(svc.cron.Entries()); n != 1 {
		t.Fatalf("expected exactly 1 cron entry, got %d", n)
	}
}

// A disabling update that the store rejects must not stop the running job.
func TestUpdateStoreFailureKeepsCronEntryWhenDisabling(t *testing.T) {
	svc, store, id, oldEntry := startedService(t)
	store.updateErr = errors.New("database is down")

	_, err := svc.Update(context.Background(), model.ScheduledCommand{
		ID: id, Name: "morning", Schedule: "0 8 * * *", Command: "electricidad", GroupJID: "123456789@g.us", Enabled: false,
	})
	if !errors.Is(err, store.updateErr) {
		t.Fatalf("expected the store error, got %v", err)
	}
	if entry, ok := svc.entryFor(id); !ok || entry != oldEntry || !svc.cron.Entry(oldEntry).Valid() {
		t.Fatal("expected the original cron entry to keep running")
	}
}

// TestRestartKeepsOneEntryPerCommand covers Start, Stop, Start: the second
// runner issues entry IDs from 1 again, so entries remembered from the first
// runner must not be treated as live.
func TestRestartKeepsOneEntryPerCommand(t *testing.T) {
	store := &fakeStore{}
	exec := &fakeExecutor{commands: []string{"electricidad", "tiempo"}}
	svc, err := New(store, exec, &fakeChecker{connected: true}, testLogger(), noop.NewTracerProvider(), Config{Timezone: "Europe/Madrid"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	ctx := context.Background()

	var enabled []uuid.UUID
	for _, schedule := range []string{"0 8 * * *", "0 9 * * *", "0 10 * * *"} {
		id, err := store.CreateScheduledCommand(ctx, model.ScheduledCommand{
			Name: "job " + schedule, Schedule: schedule, Command: "electricidad", GroupJID: "123456789@g.us", Enabled: true,
		})
		if err != nil {
			t.Fatalf("seeding failed: %v", err)
		}
		enabled = append(enabled, id)
	}
	disabled, err := store.CreateScheduledCommand(ctx, model.ScheduledCommand{
		Name: "paused", Schedule: "0 11 * * *", Command: "tiempo", GroupJID: "123456789@g.us", Enabled: false,
	})
	if err != nil {
		t.Fatalf("seeding failed: %v", err)
	}

	assertOneLiveEntryEach := func(t *testing.T) {
		t.Helper()
		if n := len(svc.cron.Entries()); n != len(enabled) {
			t.Fatalf("expected %d live cron entries, got %d", len(enabled), n)
		}
		seen := map[cron.EntryID]uuid.UUID{}
		for _, id := range enabled {
			entry, ok := svc.entryFor(id)
			if !ok {
				t.Fatalf("command %s has no cron entry", id)
			}
			if !svc.cron.Entry(entry).Valid() {
				t.Fatalf("command %s points at an entry that is not in the runner", id)
			}
			if other, dup := seen[entry]; dup {
				t.Fatalf("commands %s and %s share cron entry %d", id, other, entry)
			}
			seen[entry] = id
		}
		if _, ok := svc.entryFor(disabled); ok {
			t.Fatal("disabled command must not have a cron entry")
		}
	}

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	assertOneLiveEntryEach(t)
	svc.Stop()

	// A command deleted from the store while stopped must not survive either.
	removed := enabled[len(enabled)-1]
	if err := store.DeleteScheduledCommand(ctx, removed); err != nil {
		t.Fatalf("delete while stopped failed: %v", err)
	}
	enabled = enabled[:len(enabled)-1]

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("second Start failed: %v", err)
	}
	t.Cleanup(svc.Stop)
	assertOneLiveEntryEach(t)
	if _, ok := svc.entryFor(removed); ok {
		t.Fatal("expected no remembered entry for a command that no longer exists")
	}
}

func TestUpdateUnknownID(t *testing.T) {
	svc, _, _, _ := startedService(t)
	unknown := uuid.Must(uuid.NewV7())

	_, err := svc.Update(context.Background(), model.ScheduledCommand{
		ID: unknown, Name: "ghost", Schedule: "0 8 * * *", Command: "electricidad", GroupJID: "123456789@g.us", Enabled: true,
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
	if _, ok := svc.entryFor(unknown); ok {
		t.Fatal("expected no cron entry for an unknown command")
	}
	if n := len(svc.cron.Entries()); n != 1 {
		t.Fatalf("expected the runner to keep exactly 1 entry, got %d", n)
	}
}

func TestDeleteUnknownIDIsNotFound(t *testing.T) {
	svc, _, _, _ := startedService(t)

	err := svc.Delete(context.Background(), uuid.Must(uuid.NewV7()))
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestUpdateBeforeStartDoesNotSchedule(t *testing.T) {
	store := &fakeStore{}
	svc, err := New(store, &fakeExecutor{commands: []string{"electricidad"}}, &fakeChecker{}, testLogger(), noop.NewTracerProvider(), Config{Timezone: "Europe/Madrid"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	id, err := svc.Create(context.Background(), model.ScheduledCommand{
		Name: "morning", Schedule: "0 8 * * *", Command: "electricidad", GroupJID: "123456789@g.us",
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	got, err := svc.Update(context.Background(), model.ScheduledCommand{
		ID: id, Name: "morning", Schedule: "0 9 * * *", Command: "electricidad", GroupJID: "123456789@g.us", Enabled: true,
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if got.Schedule != "0 9 * * *" {
		t.Fatalf("unexpected schedule: %s", got.Schedule)
	}
	if _, ok := svc.entryFor(id); ok {
		t.Fatal("expected no cron entry while the runner is not started")
	}
}

// TestConcurrentMutations exercises create, update, delete and list from many
// goroutines. Its value is under -race: any unsynchronised access to the cron
// entry map fails the run.
func TestConcurrentMutations(t *testing.T) {
	svc, _, _, _ := startedService(t)
	ctx := context.Background()

	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := model.ScheduledCommand{Name: "job", Schedule: "0 8 * * *", Command: "electricidad", GroupJID: "123456789@g.us"}
			id, err := svc.Create(ctx, cmd)
			if err != nil {
				errs <- err
				return
			}
			cmd.ID = id
			cmd.Schedule = "15 9 * * *"
			cmd.Enabled = true
			if _, err := svc.Update(ctx, cmd); err != nil {
				errs <- err
				return
			}
			if _, err := svc.List(ctx); err != nil {
				errs <- err
				return
			}
			if err := svc.Delete(ctx, id); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent mutation failed: %v", err)
	}

	// Only the command created by startedService is left.
	if n := len(svc.cron.Entries()); n != 1 {
		t.Fatalf("expected 1 cron entry after all workers finished, got %d", n)
	}
}
