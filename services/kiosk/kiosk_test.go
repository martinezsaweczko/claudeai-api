package kiosk

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

type fakeUsageStore struct {
	reports [][2]string
}

func (f *fakeUsageStore) ReportUserUsage(_ context.Context, user, file string) error {
	f.reports = append(f.reports, [2]string{user, file})
	return nil
}

type fakeIssuer struct {
	lastSubject string
	lastExpiry  time.Duration
}

func (f *fakeIssuer) GenerateTokenWithExpiry(subject string, expiry time.Duration) (string, error) {
	f.lastSubject = subject
	f.lastExpiry = expiry
	return "test-token", nil
}

func newTestService(t *testing.T, dir string, retention time.Duration) (*Service, *fakeUsageStore, *fakeIssuer) {
	t.Helper()
	store := &fakeUsageStore{}
	issuer := &fakeIssuer{}

	categories := []Category{
		{
			Name: "nacional", URLPrefix: "nacional_folder", Dir: dir, Retention: retention,
			Singular: "periodico", Plural: "periodicos y revistas a nivel nacional",
			CommandWord: "periodico", RequestExample: "periodico:2",
		},
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc, err := New(categories, "files.example.com:443", store, issuer, log,
		tracenoop.NewTracerProvider(), noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}
	return svc, store, issuer
}

// writeFileWithAge creates a file with its modification time set to the given age
func writeFileWithAge(t *testing.T, dir, name string, age time.Duration) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, []byte("content"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	mtime := time.Now().Add(-age)
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		t.Fatalf("failed to set mtime: %v", err)
	}
}

func TestListFiltersByRetentionAndSortsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	writeFileWithAge(t, dir, "old.pdf", 25*time.Hour)      // older than retention -> filtered
	writeFileWithAge(t, dir, "middle.pdf", 5*time.Hour)    // 2nd newest
	writeFileWithAge(t, dir, "newest.pdf", 1*time.Hour)    // newest first
	writeFileWithAge(t, dir, "boundary.pdf", 23*time.Hour) // within retention

	svc, _, _ := newTestService(t, dir, 24*time.Hour)

	files, err := svc.List(context.Background(), "nacional")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(files) != 3 {
		t.Fatalf("expected 3 files within retention, got %d: %+v", len(files), files)
	}
	if files[0].Name != "newest.pdf" || files[1].Name != "middle.pdf" || files[2].Name != "boundary.pdf" {
		t.Errorf("files not sorted newest first: %+v", files)
	}
}

func TestListUnknownCategory(t *testing.T) {
	svc, _, _ := newTestService(t, t.TempDir(), time.Hour)
	if _, err := svc.List(context.Background(), "unknown"); err == nil {
		t.Fatal("expected error for unknown category")
	}
}

func TestListMessageFormat(t *testing.T) {
	dir := t.TempDir()
	writeFileWithAge(t, dir, "abc.pdf", time.Hour)
	writeFileWithAge(t, dir, "def.pdf", 2*time.Hour)

	svc, _, _ := newTestService(t, dir, 24*time.Hour)
	msg, err := svc.ListMessage(context.Background(), "nacional")
	if err != nil {
		t.Fatalf("ListMessage failed: %v", err)
	}

	for _, want := range []string{"*Lista de periodicos y revistas a nivel nacional.*", "periodico:2", "num:*0* abc.pdf", "num:*1* def.pdf"} {
		if !strings.Contains(msg, want) {
			t.Errorf("list message missing %q:\n%s", want, msg)
		}
	}
}

func TestLinkBuildsURLAndReportsUsage(t *testing.T) {
	dir := t.TempDir()
	writeFileWithAge(t, dir, "my file.pdf", time.Hour) // space must be URL-encoded

	svc, store, issuer := newTestService(t, dir, 24*time.Hour)

	link, fileName, err := svc.Link(context.Background(), "nacional", 0, "David")
	if err != nil {
		t.Fatalf("Link failed: %v", err)
	}

	if fileName != "my file.pdf" {
		t.Errorf("unexpected file name: %s", fileName)
	}
	if !strings.HasPrefix(link, "https://files.example.com:443/nacional_folder/") {
		t.Errorf("unexpected link prefix: %s", link)
	}
	if !strings.Contains(link, "my%20file.pdf") {
		t.Errorf("file name not URL-encoded: %s", link)
	}
	if !strings.Contains(link, "access_token=test-token") {
		t.Errorf("missing access token: %s", link)
	}
	if issuer.lastSubject != "periodico" || issuer.lastExpiry != linkTokenTTL {
		t.Errorf("unexpected token params: subject=%s expiry=%v", issuer.lastSubject, issuer.lastExpiry)
	}
	if len(store.reports) != 1 || store.reports[0] != [2]string{"David", "my file.pdf"} {
		t.Errorf("usage not reported: %+v", store.reports)
	}
}

func TestLinkOutOfRange(t *testing.T) {
	dir := t.TempDir()
	writeFileWithAge(t, dir, "only.pdf", time.Hour)

	svc, _, _ := newTestService(t, dir, 24*time.Hour)
	if _, _, err := svc.Link(context.Background(), "nacional", 5, "David"); err == nil {
		t.Fatal("expected out-of-range error")
	}
}

func TestParseIndex(t *testing.T) {
	tests := []struct {
		body    string
		want    int
		wantErr bool
	}{
		{"periodico:23", 23, false},
		{"Periodico: 5", 5, false},
		{"periódico:12", 12, false},
		{"newspaper:0", 0, false},
		{"magazine: 7", 7, false},
		{"periodico:", 0, true},
		{"periodico:abc", 0, true},
	}

	for _, tc := range tests {
		got, err := ParseIndex(tc.body)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseIndex(%q): expected error, got %d", tc.body, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseIndex(%q): unexpected error %v", tc.body, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseIndex(%q) = %d, want %d", tc.body, got, tc.want)
		}
	}
}

func TestFilePathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	if _, err := FilePath(dir, "../etc/passwd"); err == nil {
		t.Fatal("expected traversal rejection")
	}
	writeFileWithAge(t, dir, "ok.pdf", 0)
	if _, err := FilePath(dir, "ok.pdf"); err != nil {
		t.Fatalf("expected valid file, got %v", err)
	}
}
