package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/martinezsaweczko/whatsappBot-golang/model"
)

// newTestDB creates a repository backed by a temporary SQLite database
func newTestDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	db, err := New(Config{
		Driver:     "sqlite",
		DSN:        fmt.Sprintf("file:%s?_foreign_keys=on", dbPath),
		AutoCreate: true,
	}, log)
	if err != nil {
		t.Fatalf("failed to create test DB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSubscriptionsLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Empty initially
	subs, err := db.ReturnSubscriptions(ctx, []string{"user1@c.us"})
	if err != nil {
		t.Fatalf("ReturnSubscriptions failed: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("expected 0 subscriptions, got %d", len(subs))
	}

	// Save two
	if err := db.SaveSubscription(ctx, "Mundo", "user1@c.us"); err != nil {
		t.Fatalf("SaveSubscription failed: %v", err)
	}
	if err := db.SaveSubscription(ctx, "País", "user1@c.us"); err != nil {
		t.Fatalf("SaveSubscription failed: %v", err)
	}
	if err := db.SaveSubscription(ctx, "Mundo", "user2@c.us"); err != nil {
		t.Fatalf("SaveSubscription failed: %v", err)
	}

	subs, err = db.ReturnSubscriptions(ctx, []string{"user1@c.us"})
	if err != nil {
		t.Fatalf("ReturnSubscriptions failed: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("expected 2 subscriptions, got %d", len(subs))
	}

	// Delete only user1's
	if err := db.DeleteSubscription(ctx, []string{"user1@c.us"}); err != nil {
		t.Fatalf("DeleteSubscription failed: %v", err)
	}
	subs, _ = db.ReturnSubscriptions(ctx, []string{"user1@c.us"})
	if len(subs) != 0 {
		t.Fatalf("expected 0 subscriptions after delete, got %d", len(subs))
	}
	subs, _ = db.ReturnSubscriptions(ctx, []string{"user2@c.us"})
	if len(subs) != 1 {
		t.Fatalf("user2 subscriptions should be untouched, got %d", len(subs))
	}
}

// seedIdentitySubscriptions stores rows for one user under three equivalent
// identities plus one row of an unrelated user
func seedIdentitySubscriptions(t *testing.T, db *DB) {
	t.Helper()
	rows := [][2]string{
		{"Mundo", "34600111222@c.us"},
		{"Pais", "34600111222@s.whatsapp.net"},
		{"Marca", "111222333444555@lid"},
		{"Economist", "34600999888@c.us"},
	}
	for _, row := range rows {
		if err := db.SaveSubscription(context.Background(), row[0], row[1]); err != nil {
			t.Fatalf("SaveSubscription failed: %v", err)
		}
	}
}

// subscriptionTexts returns the sorted subscription texts stored in the table
func subscriptionTexts(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.db.Query("SELECT subscription_text FROM subscriptions ORDER BY subscription_text")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()

	var texts []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		texts = append(texts, text)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows failed: %v", err)
	}
	return texts
}

func TestReturnSubscriptionsByIdentitySet(t *testing.T) {
	tests := []struct {
		name  string
		users []string
		want  []string
	}{
		{
			name:  "several identities of the same user",
			users: []string{"34600111222@s.whatsapp.net", "34600111222@c.us", "111222333444555@lid"},
			want:  []string{"Marca", "Mundo", "Pais"},
		},
		{
			name:  "subset of the identities",
			users: []string{"34600111222@s.whatsapp.net", "34600111222@c.us"},
			want:  []string{"Mundo", "Pais"},
		},
		{
			name:  "single identity",
			users: []string{"111222333444555@lid"},
			want:  []string{"Marca"},
		},
		{
			name:  "identities without rows",
			users: []string{"34600000000@s.whatsapp.net", "34600000000@c.us"},
			want:  nil,
		},
		{
			name:  "empty set",
			users: []string{},
			want:  nil,
		},
		{
			name:  "nil set",
			users: nil,
			want:  nil,
		},
	}

	db := newTestDB(t)
	seedIdentitySubscriptions(t, db)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subs, err := db.ReturnSubscriptions(context.Background(), tt.users)
			if err != nil {
				t.Fatalf("ReturnSubscriptions failed: %v", err)
			}

			var got []string
			for _, sub := range subs {
				if !slices.Contains(tt.users, sub.User) {
					t.Errorf("returned a row of another user: %+v", sub)
				}
				got = append(got, sub.SubscriptionText)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("subscriptions = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeleteSubscriptionByIdentitySet(t *testing.T) {
	tests := []struct {
		name  string
		users []string
		want  []string // subscription texts left in the table
	}{
		{
			name:  "several identities of the same user",
			users: []string{"34600111222@s.whatsapp.net", "34600111222@c.us", "111222333444555@lid"},
			want:  []string{"Economist"},
		},
		{
			name:  "subset of the identities",
			users: []string{"34600111222@c.us", "111222333444555@lid"},
			want:  []string{"Economist", "Pais"},
		},
		{
			name:  "identities without rows",
			users: []string{"34600000000@s.whatsapp.net", "34600000000@c.us"},
			want:  []string{"Economist", "Marca", "Mundo", "Pais"},
		},
		{
			name:  "empty set",
			users: []string{},
			want:  []string{"Economist", "Marca", "Mundo", "Pais"},
		},
		{
			name:  "nil set",
			users: nil,
			want:  []string{"Economist", "Marca", "Mundo", "Pais"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newTestDB(t)
			seedIdentitySubscriptions(t, db)

			if err := db.DeleteSubscription(context.Background(), tt.users); err != nil {
				t.Fatalf("DeleteSubscription failed: %v", err)
			}
			if got := subscriptionTexts(t, db); !slices.Equal(got, tt.want) {
				t.Errorf("remaining subscriptions = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchSubscriptions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	_ = db.SaveSubscription(ctx, "economist", "user1@c.us")
	_ = db.SaveSubscription(ctx, "MUNDO", "user2@c.us")
	_ = db.SaveSubscription(ctx, "deportes", "user3@c.us")
	// user4 subscribes twice to overlapping terms; should appear once
	_ = db.SaveSubscription(ctx, "el", "user4@c.us")
	_ = db.SaveSubscription(ctx, "mundo", "user4@c.us")

	matches, err := db.MatchSubscriptions(ctx, "ElMundo_2024-01-01.pdf")
	if err != nil {
		t.Fatalf("MatchSubscriptions failed: %v", err)
	}

	found := map[string]bool{}
	for _, m := range matches {
		found[m] = true
	}

	if !found["user2@c.us"] || !found["user4@c.us"] {
		t.Errorf("expected user2 and user4 to match, got %v", matches)
	}
	if found["user1@c.us"] || found["user3@c.us"] {
		t.Errorf("user1/user3 should not match, got %v", matches)
	}

	// Case-insensitive: file in upper case
	matches, err = db.MatchSubscriptions(ctx, "THEECONOMIST_WEEKLY.PDF")
	if err != nil {
		t.Fatalf("MatchSubscriptions failed: %v", err)
	}
	if len(matches) != 1 || matches[0] != "user1@c.us" {
		t.Errorf("expected only user1 to match, got %v", matches)
	}
}

func TestTokenLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	count, err := db.CountTokenUses(ctx, "token-abc")
	if err != nil {
		t.Fatalf("CountTokenUses failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 uses, got %d", count)
	}

	for i := 1; i <= 3; i++ {
		if err := db.SaveToken(ctx, "token-abc"); err != nil {
			t.Fatalf("SaveToken failed: %v", err)
		}
		count, _ = db.CountTokenUses(ctx, "token-abc")
		if count != i {
			t.Fatalf("expected %d uses, got %d", i, count)
		}
	}

	// CleanJWT should not delete fresh tokens
	if err := db.CleanJWT(ctx); err != nil {
		t.Fatalf("CleanJWT failed: %v", err)
	}
	count, _ = db.CountTokenUses(ctx, "token-abc")
	if count != 3 {
		t.Fatalf("fresh tokens should survive cleanup, got %d", count)
	}
}

func TestUsageReports(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := db.ReportFileUsage(ctx, "file.pdf", 200); err != nil {
		t.Fatalf("ReportFileUsage failed: %v", err)
	}
	if err := db.ReportUserUsage(ctx, "David", "file.pdf"); err != nil {
		t.Fatalf("ReportUserUsage failed: %v", err)
	}

	var fileCount, userCount int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM file_usage").Scan(&fileCount); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if err := db.db.QueryRow("SELECT COUNT(*) FROM user_usage").Scan(&userCount); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if fileCount != 1 || userCount != 1 {
		t.Fatalf("expected 1/1 usage rows, got %d/%d", fileCount, userCount)
	}
}

func TestSQLInjectionResistance(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Attempt SQL injection through subscription text and user
	malicious := "'); DROP TABLE subscriptions; --"
	if err := db.SaveSubscription(ctx, malicious, "user@c.us"); err != nil {
		t.Fatalf("SaveSubscription failed: %v", err)
	}

	// Table must still exist and contain the literal malicious string
	subs, err := db.ReturnSubscriptions(ctx, []string{"user@c.us"})
	if err != nil {
		t.Fatalf("table was dropped! %v", err)
	}
	if len(subs) != 1 || subs[0].SubscriptionText != malicious {
		t.Fatalf("injection string not stored literally: %+v", subs)
	}
}

func TestValidateDBName(t *testing.T) {
	tests := []struct {
		name    string
		dbName  string
		wantErr bool
	}{
		{"valid letters", "whatsappbot", false},
		{"valid underscore", "whatsapp_bot", false},
		{"valid hyphen", "whatsapp-bot", false},
		{"empty", "", true},
		{"invalid space", "whatsapp bot", true},
		{"invalid semicolon", "whatsapp;bot", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDBName(tc.dbName)
			if tc.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCreateMySQLDatabaseIfNeededInvalidDSN(t *testing.T) {
	err := createMySQLDatabaseIfNeeded("not-a-valid-dsn")
	if err == nil {
		t.Fatal("expected error for invalid DSN")
	}
}

func TestScanUUID(t *testing.T) {
	db, err := New(Config{Driver: "sqlite", DSN: "file::memory:?_foreign_keys=on"}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if err != nil {
		t.Fatalf("failed to create test DB: %v", err)
	}
	defer db.Close()

	u := uuid.Must(uuid.NewV7())

	fromString, err := db.scanUUID(u.String())
	if err != nil {
		t.Fatalf("scanUUID string failed: %v", err)
	}
	if fromString != u {
		t.Fatalf("scanUUID string mismatch: got %s, want %s", fromString, u)
	}

	fromBytes, err := db.scanUUID(u[:])
	if err != nil {
		t.Fatalf("scanUUID bytes failed: %v", err)
	}
	if fromBytes != u {
		t.Fatalf("scanUUID bytes mismatch: got %s, want %s", fromBytes, u)
	}
}

func TestScheduledCommandsLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	cmd := model.ScheduledCommand{
		Name:     "morning electricity",
		Schedule: "0 8 * * *",
		Command:  "electricidad",
		GroupJID: "123456789@g.us",
		Enabled:  true,
	}

	id, err := db.CreateScheduledCommand(ctx, cmd)
	if err != nil {
		t.Fatalf("CreateScheduledCommand failed: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("expected non-nil id")
	}

	cmds, err := db.ListScheduledCommands(ctx)
	if err != nil {
		t.Fatalf("ListScheduledCommands failed: %v", err)
	}
	if len(cmds) != 1 {
		t.Fatalf("expected 1 scheduled command, got %d", len(cmds))
	}
	if cmds[0].Name != cmd.Name {
		t.Fatalf("unexpected name: %s", cmds[0].Name)
	}
	if cmds[0].ID != id {
		t.Fatalf("unexpected id: got %s, want %s", cmds[0].ID, id)
	}

	if err := db.DeleteScheduledCommand(ctx, id); err != nil {
		t.Fatalf("DeleteScheduledCommand failed: %v", err)
	}

	cmds, err = db.ListScheduledCommands(ctx)
	if err != nil {
		t.Fatalf("ListScheduledCommands after delete failed: %v", err)
	}
	if len(cmds) != 0 {
		t.Fatalf("expected 0 scheduled commands after delete, got %d", len(cmds))
	}
}

func TestGetScheduledCommand(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	want := model.ScheduledCommand{
		Name:     "morning electricity",
		Schedule: "0 8 * * *",
		Command:  "electricidad",
		GroupJID: "123456789@g.us",
		Enabled:  true,
	}
	id, err := db.CreateScheduledCommand(ctx, want)
	if err != nil {
		t.Fatalf("CreateScheduledCommand failed: %v", err)
	}

	got, err := db.GetScheduledCommand(ctx, id)
	if err != nil {
		t.Fatalf("GetScheduledCommand failed: %v", err)
	}
	if got.ID != id || got.Name != want.Name || got.Schedule != want.Schedule ||
		got.Command != want.Command || got.GroupJID != want.GroupJID || got.Enabled != want.Enabled {
		t.Fatalf("unexpected scheduled command: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("expected timestamps to be set, got created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}

	if _, err := db.GetScheduledCommand(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows for unknown id, got %v", err)
	}
}

func TestUpdateScheduledCommand(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	id, err := db.CreateScheduledCommand(ctx, model.ScheduledCommand{
		Name:     "morning electricity",
		Schedule: "0 8 * * *",
		Command:  "electricidad",
		GroupJID: "123456789@g.us",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateScheduledCommand failed: %v", err)
	}
	// A second row proves the update is scoped to the requested ID.
	otherID, err := db.CreateScheduledCommand(ctx, model.ScheduledCommand{
		Name: "other", Schedule: "0 9 * * *", Command: "tiempo", GroupJID: "999@g.us", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateScheduledCommand (other) failed: %v", err)
	}

	// Age the row so the refreshed updated_date is observable despite the
	// one-second resolution of CURRENT_TIMESTAMP.
	if _, err := db.db.ExecContext(ctx,
		"UPDATE scheduled_commands SET created_date = '2020-01-01 00:00:00', updated_date = '2020-01-01 00:00:00' WHERE id = ?",
		db.uuidValue(id)); err != nil {
		t.Fatalf("failed to age the row: %v", err)
	}
	before, err := db.GetScheduledCommand(ctx, id)
	if err != nil {
		t.Fatalf("GetScheduledCommand failed: %v", err)
	}

	update := model.ScheduledCommand{
		ID:       id,
		Name:     "evening electricity",
		Schedule: "30 20 * * 1-5",
		Command:  "electricidad mañana",
		GroupJID: "987654321@g.us",
		Enabled:  false,
	}

	// The second pass is a no-op update: it must still succeed, because
	// existence is decided by a read and not by the affected-row count.
	for _, pass := range []string{"first update", "identical update"} {
		t.Run(pass, func(t *testing.T) {
			returned, err := db.UpdateScheduledCommand(ctx, update)
			if err != nil {
				t.Fatalf("UpdateScheduledCommand failed: %v", err)
			}
			got, err := db.GetScheduledCommand(ctx, id)
			if err != nil {
				t.Fatalf("GetScheduledCommand failed: %v", err)
			}
			// The returned row is the stored row: new values and the
			// timestamps the database assigned, not the caller's input.
			if returned.ID != id || returned.Name != update.Name || returned.Schedule != update.Schedule ||
				returned.Command != update.Command || returned.GroupJID != update.GroupJID || returned.Enabled != update.Enabled {
				t.Fatalf("returned row does not carry the new values: %+v", returned)
			}
			if !returned.CreatedAt.Equal(got.CreatedAt) || !returned.UpdatedAt.Equal(got.UpdatedAt) {
				t.Fatalf("returned timestamps differ from the stored ones: returned %v/%v, stored %v/%v",
					returned.CreatedAt, returned.UpdatedAt, got.CreatedAt, got.UpdatedAt)
			}
			if got.Name != update.Name || got.Schedule != update.Schedule || got.Command != update.Command ||
				got.GroupJID != update.GroupJID || got.Enabled != update.Enabled {
				t.Fatalf("fields were not updated: %+v", got)
			}
			if !got.CreatedAt.Equal(before.CreatedAt) {
				t.Fatalf("created_date changed: got %v, want %v", got.CreatedAt, before.CreatedAt)
			}
			if !got.UpdatedAt.After(before.UpdatedAt) {
				t.Fatalf("updated_date was not refreshed: got %v, before %v", got.UpdatedAt, before.UpdatedAt)
			}
		})
	}

	other, err := db.GetScheduledCommand(ctx, otherID)
	if err != nil {
		t.Fatalf("GetScheduledCommand (other) failed: %v", err)
	}
	if other.Name != "other" || !other.Enabled {
		t.Fatalf("update leaked into another row: %+v", other)
	}
}

func TestUpdateScheduledCommandUnknownID(t *testing.T) {
	db := newTestDB(t)

	got, err := db.UpdateScheduledCommand(context.Background(), model.ScheduledCommand{
		ID: uuid.Must(uuid.NewV7()), Name: "ghost", Schedule: "0 8 * * *", Command: "electricidad", GroupJID: "1@g.us", Enabled: true,
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows for unknown id, got %v", err)
	}
	if got != (model.ScheduledCommand{}) {
		t.Fatalf("expected a zero command for unknown id, got %+v", got)
	}
	if cmds, err := db.ListScheduledCommands(context.Background()); err != nil || len(cmds) != 0 {
		t.Fatalf("expected no rows to be created, got %d (err %v)", len(cmds), err)
	}
}
