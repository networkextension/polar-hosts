package hosts

// Tests for the /internal/v1/hosts/hello handler + the store helper
// it wraps. The loopback gate + body validation are pure-handler
// concerns (no DB). The persist + unknown-host-id paths use
// go-sqlmock so we don't need a live polar_hosts DB.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func newHelloTestRouter(p *Plugin) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/internal/v1/hosts/hello", p.handleInternalHostsHello)
	return r
}

// helloRequest builds a request with the given remote addr so the
// loopback gate is exercised end-to-end.
func helloRequest(t *testing.T, remoteAddr string, body any) *http.Request {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts/hello", bytes.NewReader(buf))
	req.RemoteAddr = remoteAddr
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestInternalHostsHello_RejectsNonLoopback(t *testing.T) {
	// Public IP — the loopback gate is the only thing standing between
	// "anyone can stamp host_info" and us, so if this ever regresses we
	// want a loud failure.
	p := &Plugin{}
	r := newHelloTestRouter(p)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, helloRequest(t, "10.0.0.7:54321", map[string]any{"host_id": "h_x"}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-loopback should 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestInternalHostsHello_RejectsMalformedHostInfo(t *testing.T) {
	p := &Plugin{}
	r := newHelloTestRouter(p)
	w := httptest.NewRecorder()
	// host_info is a raw string of "not json {{{". Gin's binding will
	// accept it as raw bytes (we use json.RawMessage so the outer
	// envelope parses); then our own json.Valid catches the inner garbage.
	body := []byte(`{"host_id":"h_abc","host_info":not-json-here}`)
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts/hello", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:55555"
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed body should 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestInternalHostsHello_RequiresHostID(t *testing.T) {
	p := &Plugin{}
	r := newHelloTestRouter(p)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, helloRequest(t, "127.0.0.1:1234", map[string]any{
		// host_id intentionally omitted.
		"host_info": map[string]any{"virt": "kvm"},
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing host_id should 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestInternalHostsHello_HappyPath(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	p := &Plugin{DB: db}
	r := newHelloTestRouter(p)

	hostInfo := map[string]any{"virt": "kvm", "memory_bytes": 16e9, "kernel": "5.15"}
	hostInfoBytes, _ := json.Marshal(hostInfo)
	seen := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)

	mock.ExpectExec(`UPDATE hosts`).
		WithArgs("h_real", string(hostInfoBytes), seen).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, helloRequest(t, "127.0.0.1:6000", map[string]any{
		"host_id":       "h_real",
		"host_info":     hostInfo,
		"hello_seen_at": seen,
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("happy path should 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		OK      bool   `json:"ok"`
		HostID  string `json:"host_id"`
		Updated bool   `json:"updated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if !resp.OK || !resp.Updated || resp.HostID != "h_real" {
		t.Errorf("unexpected resp: %+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestInternalHostsHello_UnknownHostID(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	p := &Plugin{DB: db}
	r := newHelloTestRouter(p)

	// Zero rows affected = host_id not found. We want 200 (fail-soft)
	// with updated=false, NOT 500 — a legacy agent that never ran
	// /api/hosts/register should not blow up the WS reconnect path.
	mock.ExpectExec(`UPDATE hosts`).
		WithArgs("h_ghost", `{"virt":"kvm"}`, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, helloRequest(t, "127.0.0.1:6001", map[string]any{
		"host_id":   "h_ghost",
		"host_info": map[string]any{"virt": "kvm"},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("unknown host should 200 (fail-soft), got %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		OK      bool `json:"ok"`
		Updated bool `json:"updated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if !resp.OK {
		t.Error("expected ok=true")
	}
	if resp.Updated {
		t.Error("expected updated=false for unknown host_id")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestInternalHostsHello_EmptyHostInfoStoresEmptyObject(t *testing.T) {
	// Old agent build with no host_info knowledge sends an empty
	// payload (or omits it). We want to record "yes, we saw a hello
	// from this host" without leaving the column NULL.
	db, mock := newMockDB(t)
	defer db.Close()

	p := &Plugin{DB: db}
	r := newHelloTestRouter(p)

	mock.ExpectExec(`UPDATE hosts`).
		WithArgs("h_legacy", "{}", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, helloRequest(t, "127.0.0.1:6002", map[string]any{
		"host_id": "h_legacy",
		// host_info omitted entirely
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("empty host_info should 200, got %d body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// newMockDB wires a sql.DB backed by go-sqlmock so we can exercise the
// updateHostInfo persist path without a live polar_hosts DB.
func newMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return db, mock
}
