package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
)

type fakeTokenStore struct {
	counts    map[string]int
	saved     []string
	cleanCall int
}

func newFakeTokenStore() *fakeTokenStore {
	return &fakeTokenStore{counts: map[string]int{}}
}

func (f *fakeTokenStore) CountTokenUses(_ context.Context, jwt string) (int, error) {
	return f.counts[jwt], nil
}

func (f *fakeTokenStore) SaveToken(_ context.Context, jwt string) error {
	f.saved = append(f.saved, jwt)
	f.counts[jwt]++
	return nil
}

func (f *fakeTokenStore) CleanJWT(_ context.Context) error {
	f.cleanCall++
	return nil
}

type fakeUsageReporter struct {
	reports map[string]int
}

func (f *fakeUsageReporter) ReportFileUsage(_ context.Context, file string, result int) error {
	if f.reports == nil {
		f.reports = map[string]int{}
	}
	f.reports[file] = result
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newRequestWithPathValue creates a request carrying the {file} path value,
// simulating what ServeMux provides to handlers
func newRequestWithPathValue(target, file string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.SetPathValue("file", file)
	return req
}

func TestJWTQueryMiddleware(t *testing.T) {
	rejected, err := noop.NewMeterProvider().Meter("test").Int64Counter("rejected")
	if err != nil {
		t.Fatalf("failed to create counter: %v", err)
	}

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("file-content"))
	})

	tests := []struct {
		name       string
		target     string
		verifyErr  error
		priorUses  int
		wantStatus int
		wantReason string
	}{
		{name: "missing token", target: "/nacional_folder/a.pdf", wantStatus: http.StatusUnauthorized, wantReason: "missing token"},
		{name: "invalid token", target: "/nacional_folder/a.pdf?access_token=bad", verifyErr: errors.New("bad signature"), wantStatus: http.StatusUnauthorized, wantReason: "invalid"},
		{name: "valid token", target: "/nacional_folder/a.pdf?access_token=good", wantStatus: http.StatusOK},
		{name: "second use allowed", target: "/nacional_folder/a.pdf?access_token=good", priorUses: 1, wantStatus: http.StatusOK},
		{name: "third use allowed (<=2 records)", target: "/nacional_folder/a.pdf?access_token=good", priorUses: 2, wantStatus: http.StatusOK},
		{name: "replay rejected", target: "/nacional_folder/a.pdf?access_token=good", priorUses: 3, wantStatus: http.StatusUnauthorized, wantReason: "replay"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeTokenStore()
			if tc.priorUses > 0 {
				store.counts["good"] = tc.priorUses
			}
			usage := &fakeUsageReporter{}

			verify := func(token string) error { return tc.verifyErr }
			mw := JWTQueryMiddleware(testLogger(), verify, store, usage, rejected)
			handler := mw(okHandler)

			req := newRequestWithPathValue(tc.target, "a.pdf")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}

			if tc.wantStatus == http.StatusUnauthorized {
				if usage.reports["a.pdf"] != http.StatusUnauthorized {
					t.Errorf("401 not reported: %+v", usage.reports)
				}
			} else {
				// Token usage recorded + cleanup triggered
				if len(store.saved) == 0 {
					t.Error("token use not recorded")
				}
				if store.cleanCall == 0 {
					t.Error("CleanJWT not called")
				}
			}
		})
	}
}
