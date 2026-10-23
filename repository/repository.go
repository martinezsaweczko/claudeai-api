// Package repository contains the data access layer.
// All queries use parameterized statements.
package repository

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"strings"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/martinezsaweczko/whatsappBot-golang/model"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	_ "github.com/martinezsaweczko/whatsappBot-golang/repository/migrations"
)

//go:embed migrations/*/*.sql
var migrationsFS embed.FS

// newUUID returns a time-ordered UUID v7.
func newUUID() uuid.UUID {
	return uuid.Must(uuid.NewV7())
}

// uuidValue returns the UUID in the format expected by the active driver:
// BINARY(16) bytes for MySQL, text for SQLite.
func (d *DB) uuidValue(u uuid.UUID) interface{} {
	if d.dialect == "mysql" {
		return u[:]
	}
	return u.String()
}

// scanUUID reads a UUID from a database value (string for SQLite, []byte for MySQL).
func (d *DB) scanUUID(src interface{}) (uuid.UUID, error) {
	switch v := src.(type) {
	case string:
		return uuid.Parse(v)
	case []byte:
		if len(v) == 16 {
			var u uuid.UUID
			copy(u[:], v)
			return u, nil
		}
		return uuid.Parse(string(v))
	case nil:
		return uuid.Nil, fmt.Errorf("uuid value is null")
	default:
		return uuid.Nil, fmt.Errorf("unsupported uuid source type %T", src)
	}
}

// Config holds the database connection and migration settings.
type Config struct {
	Driver     string // "sqlite" or "mysql"
	DSN        string // driver-specific data source name
	AutoCreate bool   // automatically create the MySQL database if it does not exist
}

// DB provides access to the application database.
// It implements the store interfaces consumed by the services and HTTP layers.
type DB struct {
	db      *sql.DB
	dialect string
	log     *slog.Logger
}

// createMySQLDatabaseIfNeeded parses the MySQL DSN, connects without a database
// name and creates the database if it does not already exist.
func createMySQLDatabaseIfNeeded(dsn string) error {
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		return fmt.Errorf("failed to parse mysql dsn: %w", err)
	}

	dbName := cfg.DBName
	if dbName == "" {
		return nil
	}

	cfg.DBName = ""
	adminDSN := cfg.FormatDSN()

	db, err := sql.Open("mysql", adminDSN)
	if err != nil {
		return fmt.Errorf("failed to open mysql admin connection: %w", err)
	}
	defer db.Close()

	if err := validateDBName(dbName); err != nil {
		return err
	}

	_, err = db.Exec(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", dbName))
	if err != nil {
		return fmt.Errorf("failed to create database %q: %w", dbName, err)
	}
	return nil
}

// validateDBName ensures the database name only contains safe characters.
func validateDBName(name string) error {
	if name == "" {
		return fmt.Errorf("database name is empty")
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("invalid character %q in database name %q", r, name)
	}
	return nil
}

// New opens the database, runs pending migrations, and returns a repository.
func New(cfg Config, log *slog.Logger) (*DB, error) {
	if cfg.Driver == "" {
		return nil, fmt.Errorf("database driver is required")
	}
	if cfg.DSN == "" {
		return nil, fmt.Errorf("database DSN is required")
	}

	if cfg.Driver == "mysql" && cfg.AutoCreate {
		if err := createMySQLDatabaseIfNeeded(cfg.DSN); err != nil {
			return nil, fmt.Errorf("failed to create mysql database: %w", err)
		}
	}

	db, err := sql.Open(cfg.Driver, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if cfg.Driver == "sqlite" {
		// SQLite does not support concurrent writers; a single connection avoids SQLITE_BUSY errors
		db.SetMaxOpenConns(1)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	d := &DB{db: db, dialect: cfg.Driver, log: log}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, err
	}

	log.Info("Connected successfully to DB", "driver", cfg.Driver)
	return d, nil
}

// migrate runs pending goose migrations from the embedded driver-specific directory.
func (d *DB) migrate() error {
	goose.SetBaseFS(migrationsFS)

	var gooseDialect string
	switch d.dialect {
	case "mysql":
		gooseDialect = "mysql"
	default:
		gooseDialect = "sqlite3"
	}
	if err := goose.SetDialect(gooseDialect); err != nil {
		return fmt.Errorf("failed to set goose dialect %q: %w", gooseDialect, err)
	}

	path := "migrations/" + d.dialect
	if err := goose.Up(d.db, path); err != nil {
		return fmt.Errorf("failed to run migrations from %s: %w", path, err)
	}
	d.log.Debug("Database migrations applied", "path", path)
	return nil
}

// Close closes the underlying database.
func (d *DB) Close() error {
	return d.db.Close()
}

// SaveSubscription stores a new keyword subscription for a user.
func (d *DB) SaveSubscription(ctx context.Context, subscriptionText, user string) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO subscriptions (id, subscription_text, user) VALUES (?, ?, ?)",
		d.uuidValue(newUUID()), subscriptionText, user)
	if err != nil {
		return fmt.Errorf("failed to save subscription: %w", err)
	}
	return nil
}

// userInClause builds a parameterized "user IN (?, ...)" condition and its
// arguments for a non-empty set of user identities.
func userInClause(users []string) (string, []interface{}) {
	args := make([]interface{}, len(users))
	for i, user := range users {
		args[i] = user
	}
	return "user IN (?" + strings.Repeat(", ?", len(users)-1) + ")", args
}

// DeleteSubscription removes all subscriptions stored under any of the given
// user identities. An empty set deletes nothing.
func (d *DB) DeleteSubscription(ctx context.Context, users []string) error {
	if len(users) == 0 {
		return nil
	}
	clause, args := userInClause(users)
	_, err := d.db.ExecContext(ctx, "DELETE FROM subscriptions WHERE "+clause, args...)
	if err != nil {
		return fmt.Errorf("failed to delete subscriptions: %w", err)
	}
	return nil
}

// ReturnSubscriptions returns all subscriptions stored under any of the given
// user identities. An empty set returns no rows.
func (d *DB) ReturnSubscriptions(ctx context.Context, users []string) ([]model.Subscription, error) {
	if len(users) == 0 {
		return nil, nil
	}
	clause, args := userInClause(users)
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, subscription_text, user, created_date, updated_date FROM subscriptions WHERE "+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query subscriptions: %w", err)
	}
	defer rows.Close()

	var subs []model.Subscription
	for rows.Next() {
		var s model.Subscription
		var rawID interface{}
		if err := rows.Scan(&rawID, &s.SubscriptionText, &s.User, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan subscription: %w", err)
		}
		s.ID, err = d.scanUUID(rawID)
		if err != nil {
			return nil, fmt.Errorf("failed to parse subscription id: %w", err)
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

// MatchSubscriptions returns the distinct users whose subscription text is contained in the given file name (case-insensitive).
func (d *DB) MatchSubscriptions(ctx context.Context, file string) ([]string, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT DISTINCT user FROM subscriptions WHERE INSTR(UPPER(?), UPPER(subscription_text)) > 0", file)
	if err != nil {
		return nil, fmt.Errorf("failed to match subscriptions: %w", err)
	}
	defer rows.Close()

	var users []string
	for rows.Next() {
		var user string
		if err := rows.Scan(&user); err != nil {
			return nil, fmt.Errorf("failed to scan matched user: %w", err)
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

// CountTokenUses returns how many times a JWT has been used (for replay protection).
func (d *DB) CountTokenUses(ctx context.Context, jwt string) (int, error) {
	var count int
	err := d.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM jwt_used WHERE jwt = ?", jwt).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count token uses: %w", err)
	}
	return count, nil
}

// SaveToken records a JWT usage.
func (d *DB) SaveToken(ctx context.Context, jwt string) error {
	_, err := d.db.ExecContext(ctx, "INSERT INTO jwt_used (id, jwt) VALUES (?, ?)", d.uuidValue(newUUID()), jwt)
	if err != nil {
		return fmt.Errorf("failed to save token: %w", err)
	}
	return nil
}

// CleanJWT removes JWT records older than 5 days.
func (d *DB) CleanJWT(ctx context.Context) error {
	var stmt string
	switch d.dialect {
	case "mysql":
		stmt = "DELETE FROM jwt_used WHERE created_date < NOW() - INTERVAL 5 DAY"
	default:
		stmt = "DELETE FROM jwt_used WHERE created_date < datetime('now', '-5 days')"
	}

	_, err := d.db.ExecContext(ctx, stmt)
	if err != nil {
		return fmt.Errorf("failed to clean old JWTs: %w", err)
	}
	return nil
}

// ReportFileUsage records an access attempt to a file with its HTTP result code.
func (d *DB) ReportFileUsage(ctx context.Context, file string, result int) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO file_usage (id, file, result) VALUES (?, ?, ?)", d.uuidValue(newUUID()), file, result)
	if err != nil {
		return fmt.Errorf("failed to report file usage: %w", err)
	}
	return nil
}

// ReportUserUsage records that a user requested a download link for a file.
func (d *DB) ReportUserUsage(ctx context.Context, user, file string) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO user_usage (id, user, file) VALUES (?, ?, ?)", d.uuidValue(newUUID()), user, file)
	if err != nil {
		return fmt.Errorf("failed to report user usage: %w", err)
	}
	return nil
}
