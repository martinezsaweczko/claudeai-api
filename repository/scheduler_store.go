package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/martinezsaweczko/whatsappBot-golang/model"
)

// CreateScheduledCommand stores a new scheduled command and assigns it a UUID v7 ID.
func (d *DB) CreateScheduledCommand(ctx context.Context, cmd model.ScheduledCommand) (uuid.UUID, error) {
	if cmd.ID == uuid.Nil {
		cmd.ID = newUUID()
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO scheduled_commands (id, name, schedule, command, group_jid, enabled)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		d.uuidValue(cmd.ID), cmd.Name, cmd.Schedule, cmd.Command, cmd.GroupJID, cmd.Enabled)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to create scheduled command: %w", err)
	}
	return cmd.ID, nil
}

// scheduledCommandColumns is the column list shared by every scheduled command read.
const scheduledCommandColumns = "id, name, schedule, command, group_jid, enabled, created_date, updated_date"

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanScheduledCommand reads one row selected with scheduledCommandColumns.
// A missing row surfaces as sql.ErrNoRows, unwrapped.
func (d *DB) scanScheduledCommand(row rowScanner) (model.ScheduledCommand, error) {
	var c model.ScheduledCommand
	var rawID interface{}
	if err := row.Scan(&rawID, &c.Name, &c.Schedule, &c.Command, &c.GroupJID, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.ScheduledCommand{}, sql.ErrNoRows
		}
		return model.ScheduledCommand{}, fmt.Errorf("failed to scan scheduled command: %w", err)
	}
	id, err := d.scanUUID(rawID)
	if err != nil {
		return model.ScheduledCommand{}, fmt.Errorf("failed to parse scheduled command id: %w", err)
	}
	c.ID = id
	return c, nil
}

// ListScheduledCommands returns all scheduled commands.
func (d *DB) ListScheduledCommands(ctx context.Context) ([]model.ScheduledCommand, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT "+scheduledCommandColumns+" FROM scheduled_commands ORDER BY created_date DESC")
	if err != nil {
		return nil, fmt.Errorf("failed to list scheduled commands: %w", err)
	}
	defer rows.Close()

	var cmds []model.ScheduledCommand
	for rows.Next() {
		c, err := d.scanScheduledCommand(rows)
		if err != nil {
			return nil, err
		}
		cmds = append(cmds, c)
	}
	return cmds, rows.Err()
}

// GetScheduledCommand returns one scheduled command by ID, or sql.ErrNoRows
// when it does not exist.
func (d *DB) GetScheduledCommand(ctx context.Context, id uuid.UUID) (model.ScheduledCommand, error) {
	row := d.db.QueryRowContext(ctx,
		"SELECT "+scheduledCommandColumns+" FROM scheduled_commands WHERE id = ?", d.uuidValue(id))
	return d.scanScheduledCommand(row)
}

// UpdateScheduledCommand replaces the editable fields of the scheduled command
// identified by cmd.ID, refreshes updated_date and returns the stored row. It
// returns sql.ErrNoRows when the ID does not exist.
//
// The row is read back inside the same transaction as the UPDATE, so a nil
// error always comes with the committed row and its real timestamps; callers
// need no second read that could fail after the change is durable. That read
// also decides existence, instead of RowsAffected: MySQL reports zero affected
// rows when the new values equal the stored ones, which would make an
// identical update look like a missing row. updated_date is set explicitly
// because SQLite has no ON UPDATE clause.
func (d *DB) UpdateScheduledCommand(ctx context.Context, cmd model.ScheduledCommand) (model.ScheduledCommand, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ScheduledCommand{}, fmt.Errorf("failed to begin scheduled command update: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit

	if _, err := tx.ExecContext(ctx,
		`UPDATE scheduled_commands
		 SET name = ?, schedule = ?, command = ?, group_jid = ?, enabled = ?, updated_date = CURRENT_TIMESTAMP
		 WHERE id = ?`,
		cmd.Name, cmd.Schedule, cmd.Command, cmd.GroupJID, cmd.Enabled, d.uuidValue(cmd.ID)); err != nil {
		return model.ScheduledCommand{}, fmt.Errorf("failed to update scheduled command: %w", err)
	}

	stored, err := d.scanScheduledCommand(tx.QueryRowContext(ctx,
		"SELECT "+scheduledCommandColumns+" FROM scheduled_commands WHERE id = ?", d.uuidValue(cmd.ID)))
	if err != nil {
		// sql.ErrNoRows here means the UPDATE matched nothing: unknown ID.
		return model.ScheduledCommand{}, err
	}

	if err := tx.Commit(); err != nil {
		return model.ScheduledCommand{}, fmt.Errorf("failed to commit scheduled command update: %w", err)
	}
	return stored, nil
}

// DeleteScheduledCommand removes a scheduled command by ID.
func (d *DB) DeleteScheduledCommand(ctx context.Context, id uuid.UUID) error {
	res, err := d.db.ExecContext(ctx, "DELETE FROM scheduled_commands WHERE id = ?", d.uuidValue(id))
	if err != nil {
		return fmt.Errorf("failed to delete scheduled command: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}
