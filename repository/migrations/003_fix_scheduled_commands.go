package migrations

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// migrationNamespace is the UUID namespace used to generate deterministic
// UUIDs for legacy integer primary keys during the scheduled_commands repair.
var migrationNamespace = uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")

func init() {
	goose.AddMigrationContext(up003, down003)
}

func up003(ctx context.Context, tx *sql.Tx) error {
	dialect, err := detectDialect(ctx, tx)
	if err != nil {
		return fmt.Errorf("failed to detect dialect: %w", err)
	}

	if err := addUpdatedDate(ctx, tx, dialect); err != nil {
		return fmt.Errorf("failed to add updated_date: %w", err)
	}

	needsMigration, err := needsIDMigration(ctx, tx, dialect)
	if err != nil {
		return fmt.Errorf("failed to check scheduled_commands id column: %w", err)
	}
	if !needsMigration {
		return nil
	}

	if err := migrateIDs(ctx, tx, dialect); err != nil {
		return fmt.Errorf("failed to migrate scheduled_commands ids: %w", err)
	}
	return nil
}

func down003(ctx context.Context, tx *sql.Tx) error {
	// The up migration performs a one-way repair of a schema that was
	// inconsistent with the application code; a safe reverse migration is not
	// feasible because legacy integer ids cannot be recovered from the
	// deterministic UUIDs that replaced them.
	return nil
}

func detectDialect(ctx context.Context, tx *sql.Tx) (string, error) {
	var one int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master LIMIT 1").Scan(&one); err == nil {
		return "sqlite", nil
	}
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM information_schema.tables LIMIT 1").Scan(&one); err == nil {
		return "mysql", nil
	}
	return "", fmt.Errorf("could not detect database dialect")
}

func addUpdatedDate(ctx context.Context, tx *sql.Tx, dialect string) error {
	exists, err := columnExists(ctx, tx, dialect, "scheduled_commands", "updated_date")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	var stmt string
	switch dialect {
	case "mysql":
		stmt = "ALTER TABLE scheduled_commands ADD COLUMN updated_date DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP"
	case "sqlite":
		// SQLite's ALTER TABLE ADD COLUMN only permits constant defaults; the
		// repair migration migrates data into a new table with the correct
		// default immediately afterwards.
		stmt = "ALTER TABLE scheduled_commands ADD COLUMN updated_date DATETIME"
	default:
		return fmt.Errorf("unsupported dialect %q", dialect)
	}
	_, err = tx.ExecContext(ctx, stmt)
	return err
}

func columnExists(ctx context.Context, tx *sql.Tx, dialect, table, column string) (bool, error) {
	switch dialect {
	case "mysql":
		var count int
		err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM information_schema.columns
			WHERE table_schema = DATABASE()
			  AND table_name = ?
			  AND column_name = ?
		`, table, column).Scan(&count)
		return count > 0, err
	case "sqlite":
		var count int
		err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?",
			table, column,
		).Scan(&count)
		return count > 0, err
	default:
		return false, fmt.Errorf("unsupported dialect %q", dialect)
	}
}

func needsIDMigration(ctx context.Context, tx *sql.Tx, dialect string) (bool, error) {
	switch dialect {
	case "mysql":
		var dataType string
		err := tx.QueryRowContext(ctx, `
			SELECT data_type
			FROM information_schema.columns
			WHERE table_schema = DATABASE()
			  AND table_name = 'scheduled_commands'
			  AND column_name = 'id'
		`).Scan(&dataType)
		if err != nil {
			return false, err
		}
		return dataType != "binary", nil
	case "sqlite":
		var colType string
		err := tx.QueryRowContext(ctx,
			"SELECT type FROM pragma_table_info('scheduled_commands') WHERE name = 'id'",
		).Scan(&colType)
		if err != nil {
			return false, err
		}
		return colType != "TEXT", nil
	default:
		return false, fmt.Errorf("unsupported dialect %q", dialect)
	}
}

func migrateIDs(ctx context.Context, tx *sql.Tx, dialect string) error {
	const newTableMySQL = `
CREATE TABLE scheduled_commands_new (
    id BINARY(16) PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    schedule VARCHAR(255) NOT NULL,
    command VARCHAR(255) NOT NULL,
    group_jid VARCHAR(255) NOT NULL,
    enabled BOOLEAN DEFAULT TRUE,
    created_date DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_date DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_scheduled_commands_enabled (enabled)
)`
	const newTableSQLite = `
CREATE TABLE scheduled_commands_new (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    schedule TEXT NOT NULL,
    command TEXT NOT NULL,
    group_jid TEXT NOT NULL,
    enabled BOOLEAN DEFAULT 1,
    created_date DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_date DATETIME DEFAULT CURRENT_TIMESTAMP
)`
	const insertMySQL = `
INSERT INTO scheduled_commands_new
    (id, name, schedule, command, group_jid, enabled, created_date, updated_date)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	const insertSQLite = `
INSERT INTO scheduled_commands_new
    (id, name, schedule, command, group_jid, enabled, created_date, updated_date)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

	var createStmt, insertStmt string
	switch dialect {
	case "mysql":
		createStmt, insertStmt = newTableMySQL, insertMySQL
	case "sqlite":
		createStmt, insertStmt = newTableSQLite, insertSQLite
	default:
		return fmt.Errorf("unsupported dialect %q", dialect)
	}

	// Drop a leftover new table from a previous failed run so the repair is
	// idempotent even when DDL statements are implicitly committed.
	if _, err := tx.ExecContext(ctx, "DROP TABLE IF EXISTS scheduled_commands_new"); err != nil {
		return fmt.Errorf("failed to clean up stale scheduled_commands_new table: %w", err)
	}

	if _, err := tx.ExecContext(ctx, createStmt); err != nil {
		return fmt.Errorf("failed to create new scheduled_commands table: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, name, schedule, command, group_jid, enabled,
		       COALESCE(created_date, CURRENT_TIMESTAMP),
		       COALESCE(updated_date, CURRENT_TIMESTAMP)
		FROM scheduled_commands
		ORDER BY created_date ASC`)
	if err != nil {
		return fmt.Errorf("failed to read existing scheduled commands: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var rawID interface{}
		var name, schedule, command, groupJID string
		var enabled bool
		var createdAt, updatedAt string
		if err := rows.Scan(&rawID, &name, &schedule, &command, &groupJID, &enabled, &createdAt, &updatedAt); err != nil {
			return fmt.Errorf("failed to scan scheduled command: %w", err)
		}

		newID, err := convertLegacyID(rawID)
		if err != nil {
			return fmt.Errorf("failed to convert scheduled command id: %w", err)
		}

		var idValue interface{}
		switch dialect {
		case "mysql":
			idValue = newID[:]
		case "sqlite":
			idValue = newID.String()
		}

		if _, err := tx.ExecContext(ctx, insertStmt,
			idValue, name, schedule, command, groupJID, enabled, createdAt, updatedAt,
		); err != nil {
			return fmt.Errorf("failed to insert migrated scheduled command: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed iterating scheduled commands: %w", err)
	}

	if _, err := tx.ExecContext(ctx, "DROP TABLE scheduled_commands"); err != nil {
		return fmt.Errorf("failed to drop old scheduled_commands table: %w", err)
	}

	switch dialect {
	case "mysql":
		if _, err := tx.ExecContext(ctx, "ALTER TABLE scheduled_commands_new RENAME TO scheduled_commands"); err != nil {
			return fmt.Errorf("failed to rename new scheduled_commands table: %w", err)
		}
	case "sqlite":
		if _, err := tx.ExecContext(ctx, "ALTER TABLE scheduled_commands_new RENAME TO scheduled_commands"); err != nil {
			return fmt.Errorf("failed to rename new scheduled_commands table: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_scheduled_commands_enabled ON scheduled_commands (enabled)"); err != nil {
			return fmt.Errorf("failed to recreate scheduled_commands index: %w", err)
		}
	}

	return nil
}

func convertLegacyID(raw interface{}) (uuid.UUID, error) {
	switch v := raw.(type) {
	case int64:
		return uuid.NewSHA1(migrationNamespace, []byte(fmt.Sprintf("%d", v))), nil
	case int:
		return uuid.NewSHA1(migrationNamespace, []byte(fmt.Sprintf("%d", v))), nil
	case int32:
		return uuid.NewSHA1(migrationNamespace, []byte(fmt.Sprintf("%d", v))), nil
	case string:
		u, err := uuid.Parse(v)
		if err != nil {
			return uuid.Nil, fmt.Errorf("invalid string id %q: %w", v, err)
		}
		return u, nil
	case []byte:
		if len(v) == 16 {
			var u uuid.UUID
			copy(u[:], v)
			return u, nil
		}
		u, err := uuid.Parse(string(v))
		if err != nil {
			return uuid.Nil, fmt.Errorf("invalid byte id %q: %w", string(v), err)
		}
		return u, nil
	case nil:
		return uuid.Nil, fmt.Errorf("id is null")
	default:
		return uuid.Nil, fmt.Errorf("unsupported id type %T", raw)
	}
}
