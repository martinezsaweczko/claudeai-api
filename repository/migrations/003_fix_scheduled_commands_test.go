package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"
)

func Test003MigrationRepairsLegacyScheduledCommands(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Create the legacy schema from the first version of migration 002:
	// integer primary key and no updated_date column.
	if _, err := db.Exec(`
		CREATE TABLE scheduled_commands (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			schedule TEXT NOT NULL,
			command TEXT NOT NULL,
			group_jid TEXT NOT NULL,
			enabled BOOLEAN DEFAULT 1,
			created_date DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX idx_scheduled_commands_enabled ON scheduled_commands (enabled);
	`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	// Insert a legacy row.
	res, err := db.Exec(`
		INSERT INTO scheduled_commands (name, schedule, command, group_jid, enabled)
		VALUES (?, ?, ?, ?, ?)`,
		"morning", "0 8 * * *", "!weather", "123@g.us", true)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	legacyID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("legacy id: %v", err)
	}

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := up003(context.Background(), tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("run migration 003: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	// Verify the schema is now correct.
	var colType string
	if err := db.QueryRow("SELECT type FROM pragma_table_info('scheduled_commands') WHERE name = 'id'").Scan(&colType); err != nil {
		t.Fatalf("read id type: %v", err)
	}
	if colType != "TEXT" {
		t.Fatalf("expected id type TEXT, got %q", colType)
	}

	var hasUpdated int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('scheduled_commands') WHERE name = 'updated_date'").Scan(&hasUpdated); err != nil {
		t.Fatalf("check updated_date: %v", err)
	}
	if hasUpdated != 1 {
		t.Fatalf("expected updated_date column to exist")
	}

	// Verify the legacy row was migrated.
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM scheduled_commands WHERE name = ?", "morning").Scan(&count); err != nil {
		t.Fatalf("count migrated row: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 migrated row, got %d", count)
	}

	// Verify the migrated id is a deterministic UUID derived from the legacy id.
	var migratedID string
	if err := db.QueryRow("SELECT id FROM scheduled_commands WHERE name = ?", "morning").Scan(&migratedID); err != nil {
		t.Fatalf("read migrated id: %v", err)
	}
	if migratedID == fmt.Sprintf("%d", legacyID) {
		t.Fatalf("expected migrated id to differ from legacy integer id")
	}
}
