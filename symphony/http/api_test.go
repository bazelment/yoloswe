package symphttp

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetState_WrongMethod(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))
	srv := NewServer(nil, 0, logger)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/state", nil)
	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/v1/state: got status %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}

	var errResp errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.Code != "method_not_allowed" {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, "method_not_allowed")
	}
}

func TestGetIssue_WrongMethod(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))
	srv := NewServer(nil, 0, logger)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/NONEXISTENT-123", nil)
	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/v1/NONEXISTENT-123: got status %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestRefresh_WrongMethod(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))
	srv := NewServer(nil, 0, logger)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/refresh", nil)
	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/v1/refresh: got status %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}

	var errResp errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.Code != "method_not_allowed" {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, "method_not_allowed")
	}
}

func TestDashboard_WrongMethod(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))
	srv := NewServer(nil, 0, logger)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /: got status %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestDashboard_NotFoundForOtherPaths(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))
	srv := NewServer(nil, 0, logger)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /nonexistent: got status %d, want %d", rec.Code, http.StatusNotFound)
	}
}
