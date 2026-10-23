// Package scheduler runs recurring WhatsApp commands on a cron schedule.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/martinezsaweczko/whatsappBot-golang/model"
	"github.com/robfig/cron/v3"
	"go.mau.fi/whatsmeow/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ErrInvalid marks a scheduled command rejected by validation. Callers detect
// it with errors.Is to tell a bad request apart from a storage failure.
var ErrInvalid = errors.New("invalid scheduled command")

// validationError carries a validation message and matches ErrInvalid. Its
// text is the bare message, so API responses keep the wording they always had.
type validationError struct {
	err error
}

func (e *validationError) Error() string { return e.err.Error() }

// Unwrap exposes both the sentinel and the underlying cause to errors.Is/As.
func (e *validationError) Unwrap() []error { return []error{ErrInvalid, e.err} }

// invalidf builds a validation error with fmt.Errorf semantics (including %w).
func invalidf(format string, args ...any) error {
	return &validationError{err: fmt.Errorf(format, args...)}
}

// Store persists scheduled commands. UpdateScheduledCommand returns the stored
// row, timestamps included, and reports an unknown ID as sql.ErrNoRows.
type Store interface {
	CreateScheduledCommand(ctx context.Context, cmd model.ScheduledCommand) (uuid.UUID, error)
	ListScheduledCommands(ctx context.Context) ([]model.ScheduledCommand, error)
	UpdateScheduledCommand(ctx context.Context, cmd model.ScheduledCommand) (model.ScheduledCommand, error)
	DeleteScheduledCommand(ctx context.Context, id uuid.UUID) error
}

// CommandExecutor runs a command by matching its text against registered handlers.
type CommandExecutor interface {
	ExecuteCommand(ctx context.Context, body string, chat, senderJID types.JID) error
	Match(body string) (string, error)
}

// ConnectionChecker reports whether the WhatsApp client is connected.
type ConnectionChecker interface {
	IsConnected() bool
	OwnJID() types.JID
}

// Config holds the scheduler configuration.
type Config struct {
	Timezone string
	Enabled  bool
}

// Service manages recurring scheduled commands.
type Service struct {
	store    Store
	executor CommandExecutor
	checker  ConnectionChecker
	log      *slog.Logger
	tracer   trace.Tracer
	cfg      Config
	location *time.Location

	// mu serialises Start, Create, Update and Delete, and guards cron and
	// entries. It is held across the store call of each mutation so that the
	// stored rows and the registered cron entries cannot drift apart when
	// requests for the same command interleave (the internal API is served
	// before Start runs and from concurrent goroutines afterwards).
	mu      sync.Mutex
	cron    *cron.Cron
	entries map[uuid.UUID]cron.EntryID
}

// New creates a scheduler service. It does not start the cron runner; call Start after creation.
func New(store Store, executor CommandExecutor, checker ConnectionChecker, log *slog.Logger, tp trace.TracerProvider, cfg Config) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("scheduler store is required")
	}
	if executor == nil {
		return nil, fmt.Errorf("command executor is required")
	}
	if checker == nil {
		return nil, fmt.Errorf("connection checker is required")
	}

	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("invalid scheduler timezone %q: %w", cfg.Timezone, err)
	}

	return &Service{
		store:    store,
		executor: executor,
		checker:  checker,
		log:      log,
		tracer:   tp.Tracer("services/scheduler"),
		cfg:      cfg,
		entries:  make(map[uuid.UUID]cron.EntryID),
		location: loc,
	}, nil
}

// Start loads existing schedules from the database and starts the cron runner.
func (s *Service) Start(ctx context.Context) error {
	ctx, span := s.tracer.Start(ctx, "scheduler.Start")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Entry IDs belong to one runner and every new runner numbers them from 1
	// again, so entries remembered from a previous Start must be dropped:
	// otherwise a stale ID can equal a freshly issued one and replacing it
	// would remove the job that was just registered.
	s.cron = cron.New(cron.WithLocation(s.location))
	s.entries = make(map[uuid.UUID]cron.EntryID)

	cmds, err := s.store.ListScheduledCommands(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to load scheduled commands: %w", err)
	}

	for _, cmd := range cmds {
		if !cmd.Enabled {
			continue
		}
		if err := s.setCronJobLocked(cmd); err != nil {
			// Not fatal: the remaining commands are still scheduled
			span.RecordError(err)
			s.log.Error("Failed to schedule command on startup", "id", cmd.ID, "error", err)
		}
	}

	s.cron.Start()
	span.SetAttributes(attribute.Int("schedules", len(s.entries)))
	s.log.Info("Scheduler started", "timezone", s.cfg.Timezone, "schedules", len(s.entries))
	return nil
}

// Stop halts the cron runner.
func (s *Service) Stop() {
	s.mu.Lock()
	runner := s.cron
	s.mu.Unlock()

	// Wait for running jobs without holding the lock.
	if runner != nil {
		ctx := runner.Stop()
		<-ctx.Done()
		s.log.Info("Scheduler stopped")
	}
}

// Create validates and persists a new scheduled command, then adds it to the cron runner.
func (s *Service) Create(ctx context.Context, cmd model.ScheduledCommand) (uuid.UUID, error) {
	ctx, span := s.tracer.Start(ctx, "scheduler.Create")
	defer span.End()

	if err := s.validate(cmd); err != nil {
		span.SetAttributes(attribute.String("validation_error", err.Error()))
		return uuid.Nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cmd.Enabled = true
	id, err := s.store.CreateScheduledCommand(ctx, cmd)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return uuid.Nil, fmt.Errorf("failed to store scheduled command: %w", err)
	}
	cmd.ID = id

	if s.cron != nil {
		if err := s.setCronJobLocked(cmd); err != nil {
			span.RecordError(err)
			s.log.Error("Failed to add cron job after create", "id", id.String(), "error", err)
		}
	}

	s.log.Info("Created scheduled command", "id", id.String(), "schedule", cmd.Schedule, "command", cmd.Command)
	return id, nil
}

// List returns all scheduled commands.
func (s *Service) List(ctx context.Context) ([]model.ScheduledCommand, error) {
	ctx, span := s.tracer.Start(ctx, "scheduler.List")
	defer span.End()

	cmds, err := s.store.ListScheduledCommands(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	return cmds, nil
}

// Update validates and replaces an existing scheduled command, then swaps its
// cron entry so the new schedule takes effect immediately. It returns the row
// as the store wrote it, timestamps included.
//
// Errors, in the order they can occur:
//   - a rejected command matches ErrInvalid; the store is not called;
//   - an unknown ID matches sql.ErrNoRows;
//   - any other store failure is returned wrapped.
//
// In those three cases nothing was stored and the running cron entry is left
// untouched. Once the store has committed, the only possible error is a
// failure to register the new cron entry: the row is then already updated
// while the previous entry keeps running until the next successful update or
// restart.
func (s *Service) Update(ctx context.Context, cmd model.ScheduledCommand) (model.ScheduledCommand, error) {
	ctx, span := s.tracer.Start(ctx, "scheduler.Update",
		trace.WithAttributes(attribute.String("id", cmd.ID.String())))
	defer span.End()

	if err := s.validate(cmd); err != nil {
		span.SetAttributes(attribute.String("validation_error", err.Error()))
		return model.ScheduledCommand{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	stored, err := s.store.UpdateScheduledCommand(ctx, cmd)
	if err != nil {
		span.RecordError(err)
		return model.ScheduledCommand{}, fmt.Errorf("failed to update scheduled command: %w", err)
	}

	if cmd.Enabled {
		if s.cron != nil {
			if err := s.setCronJobLocked(cmd); err != nil {
				span.RecordError(err)
				return model.ScheduledCommand{}, fmt.Errorf("scheduled command stored but not rescheduled: %w", err)
			}
		}
	} else {
		s.removeCronJobLocked(cmd.ID)
	}

	s.log.Info("Updated scheduled command", "id", cmd.ID.String(), "schedule", cmd.Schedule, "command", cmd.Command, "enabled", cmd.Enabled)
	return stored, nil
}

// Delete removes a scheduled command from the cron runner and the database.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	ctx, span := s.tracer.Start(ctx, "scheduler.Delete",
		trace.WithAttributes(attribute.String("id", id.String())))
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.removeCronJobLocked(id)

	if err := s.store.DeleteScheduledCommand(ctx, id); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to delete scheduled command: %w", err)
	}

	s.log.Info("Deleted scheduled command", "id", id.String())
	return nil
}

// validate checks that the schedule, group JID and command are valid. Every
// error it returns matches ErrInvalid.
func (s *Service) validate(cmd model.ScheduledCommand) error {
	if cmd.Name == "" {
		return invalidf("name is required")
	}
	if cmd.Schedule == "" {
		return invalidf("schedule is required")
	}
	if _, err := cron.ParseStandard(cmd.Schedule); err != nil {
		return invalidf("invalid cron expression: %w", err)
	}
	if cmd.Command == "" {
		return invalidf("command is required")
	}
	if _, err := s.executor.Match(cmd.Command); err != nil {
		return invalidf("command does not match any registered handler: %w", err)
	}
	if cmd.GroupJID == "" {
		return invalidf("group_jid is required")
	}
	groupJID, err := types.ParseJID(cmd.GroupJID)
	if err != nil || groupJID.User == "" || groupJID.Server != types.GroupServer {
		return invalidf("invalid group JID: %s", cmd.GroupJID)
	}
	return nil
}

// setCronJobLocked registers the cron job of a scheduled command, replacing
// any entry the command already had. The new entry is added first, so a
// failure leaves the previous one running. The caller must hold s.mu and
// s.cron must be non-nil.
func (s *Service) setCronJobLocked(cmd model.ScheduledCommand) error {
	id := cmd.ID
	entryID, err := s.cron.AddFunc(cmd.Schedule, func() {
		s.runJob(id, cmd.Schedule, cmd.Command, cmd.GroupJID)
	})
	if err != nil {
		return fmt.Errorf("failed to add cron job for schedule %q: %w", cmd.Schedule, err)
	}
	if oldEntryID, ok := s.entries[id]; ok {
		s.cron.Remove(oldEntryID)
	}
	s.entries[id] = entryID
	return nil
}

// removeCronJobLocked drops the cron entry of a scheduled command, if it has
// one. The caller must hold s.mu.
func (s *Service) removeCronJobLocked(id uuid.UUID) {
	entryID, ok := s.entries[id]
	if !ok {
		return
	}
	if s.cron != nil {
		s.cron.Remove(entryID)
	}
	delete(s.entries, id)
}

// runJob executes a single scheduled command.
func (s *Service) runJob(id uuid.UUID, schedule, command, groupJIDStr string) {
	ctx, span := s.tracer.Start(context.Background(), "scheduler.RunJob",
		trace.WithAttributes(
			attribute.String("id", id.String()),
			attribute.String("schedule", schedule),
			attribute.String("command", command),
			attribute.String("group_jid", groupJIDStr),
		))
	defer span.End()

	log := s.log.With("id", id, "schedule", schedule, "command", command, "group_jid", groupJIDStr)

	// robfig/cron adds no recovery and the scheduled path calls the handler
	// directly, so a panicking handler would otherwise crash the process.
	// Deferred after span.End, so it runs first and the span records it.
	defer func() {
		if rec := recover(); rec != nil {
			err := fmt.Errorf("scheduled command panicked: %v", rec)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			log.Error("Scheduled command panicked", "panic", fmt.Sprint(rec))
		}
	}()

	if !s.checker.IsConnected() {
		// Expected while the client reconnects: visible on the span, not an error
		span.SetAttributes(attribute.Bool("skipped", true))
		span.AddEvent("skipped", trace.WithAttributes(attribute.String("reason", "whatsapp_not_connected")))
		log.Warn("Skipping scheduled command: WhatsApp not connected")
		return
	}

	groupJID, err := types.ParseJID(groupJIDStr)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		log.Error("Invalid group JID in scheduled command", "error", err)
		return
	}

	ownJID := s.checker.OwnJID()
	log.Info("Executing scheduled command")

	if err := s.executor.ExecuteCommand(ctx, command, groupJID, ownJID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		log.Error("Scheduled command failed", "error", err)
		return
	}
	log.Info("Scheduled command executed successfully")
}
