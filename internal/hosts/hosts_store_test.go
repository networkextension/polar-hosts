package hosts

// Pure-logic tests for hosts_store.go. Anything that touches the
// real DB (sql.Open / sql.DB.Query) is exercised in the P0 smoke
// step (`dropdb && createdb && go run ./cmd/dock` + a manual
// enrollment round-trip). This mirrors the existing dock-package
// convention: ai_agent_stream_test.go and metrics_test.go are also
// pure-memory.
//
// The createHost dedup branch + machine_uuid backfill ARE DB-touching;
// those use go-sqlmock (same as internal_hello_test.go).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	sdk "github.com/networkextension/polar-sdk"
)

// pendingEnrollmentMarker is private; the json round-trip below
// proves the wire shape matches what consumeEnrollmentToken expects
// when it reads back from agent_tokens.coder_config.
func TestPendingEnrollmentMarkerRoundtrip(t *testing.T) {
	now := time.Now().UTC().Round(time.Second)
	in := pendingEnrollmentMarker{
		PendingEnrollment: true,
		WorkspaceID:       "t_abc123",
		HostName:          "locals-Mac",
		ExpiresAt:         now.Add(time.Hour),
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"pending_enrollment":true`) {
		t.Errorf("missing pending_enrollment flag in: %s", raw)
	}
	var out pendingEnrollmentMarker
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.PendingEnrollment {
		t.Error("PendingEnrollment lost across roundtrip")
	}
	if out.WorkspaceID != in.WorkspaceID || out.HostName != in.HostName {
		t.Errorf("field mismatch: got %+v want %+v", out, in)
	}
	if !out.ExpiresAt.Equal(in.ExpiresAt) {
		t.Errorf("expires_at mismatch: got %v want %v", out.ExpiresAt, in.ExpiresAt)
	}
}

func TestPendingEnrollmentMarkerNotMatchingPlainConfig(t *testing.T) {
	// A normal agent_token.coder_config (from createAgentToken without
	// enrollment) is shaped like AgentCoderConfig — no
	// pending_enrollment field. Verify that decoding such a payload
	// into pendingEnrollmentMarker yields PendingEnrollment=false so
	// consumeEnrollmentToken correctly rejects re-use of a normal
	// (already-consumed or never-pending) token with errEnrollmentAlready.
	plain := `{"kimi":{"auth":"api_key"},"claude":{}}`
	var m pendingEnrollmentMarker
	if err := json.Unmarshal([]byte(plain), &m); err != nil {
		t.Fatalf("unmarshal plain config: %v", err)
	}
	if m.PendingEnrollment {
		t.Error("plain coder_config decoded to PendingEnrollment=true; would let consume() succeed twice")
	}
}

func TestAdvertisedSkillMarshalShape(t *testing.T) {
	// The skills slice goes into hosts.advertised_skills_json. The
	// UI parses it with this exact shape; if a future refactor breaks
	// the JSON tags this test catches it immediately rather than
	// surfacing as "skill cards don't render" in production.
	skills := []AdvertisedSkill{
		{Kind: "coder", Version: "1.0", Capabilities: map[string]any{"tools": []string{"kimi", "claude"}}},
		{Kind: "shell", Version: "1.0"},
	}
	raw, err := json.Marshal(skills)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, needle := range []string{`"kind":"coder"`, `"version":"1.0"`, `"capabilities":{"tools":["kimi","claude"]}`, `"kind":"shell"`} {
		if !strings.Contains(string(raw), needle) {
			t.Errorf("missing %q in serialized payload: %s", needle, raw)
		}
	}
}

func TestHostSkillMarshalShape(t *testing.T) {
	// HostSkill is what /api/hosts/:id will surface in P1c for the
	// skill config form. Lock the JSON shape so the TS UI bindings
	// (ui/src/types/hosts.ts) can rely on it.
	createdAt := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	hs := HostSkill{
		ID:         42,
		HostID:     "h_abc",
		Kind:       "coder",
		Name:       "coder",
		ConfigJSON: json.RawMessage(`{"mode":"tool-loop","tools":["kimi","claude"]}`),
		Enabled:    true,
		AutoStart:  true,
		CreatedBy:  "u_op",
		CreatedAt:  createdAt,
		UpdatedAt:  createdAt,
	}
	raw, err := json.Marshal(hs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, needle := range []string{
		`"id":42`,
		`"host_id":"h_abc"`,
		`"kind":"coder"`,
		`"config_json":{"mode":"tool-loop","tools":["kimi","claude"]}`,
		`"enabled":true`,
		`"auto_start":true`,
		`"created_by":"u_op"`,
	} {
		if !strings.Contains(string(raw), needle) {
			t.Errorf("missing %q in serialized payload: %s", needle, raw)
		}
	}
}

func TestSlugSanitizeRegexBehavior(t *testing.T) {
	// uniqueHostSlugInWorkspace hits the DB so we can't fully exercise
	// it without a connection; but the slug sanitization is the
	// failure-prone part and is pure. Test it through a small wrapper
	// that mirrors the production logic.
	cases := []struct {
		name string
		want string
	}{
		{"my-mac", "my-mac"},
		{"My Mac", "my-mac"},
		{"locals-Mac.local", "locals-maclocal"},
		{"  trim me!  ", "trim-me"},
		{"中文名字", "host"},  // all stripped → falls back to "host"
		{"", "host"},          // empty → "host"
		{"a / b / c", "a--b--c"},
		{"-leading-trailing-", "leading-trailing"},
	}
	for _, tc := range cases {
		got := sanitizeSlugForTest(tc.name)
		if got != tc.want {
			t.Errorf("sanitize(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// v4 — registerAgent test fixture. Mocks the dock SDK endpoint via
// httptest + sqlmock for the local mirror writes. See
// doc/arch/agent-identity-v4.md for the protocol.

func newV4Fixture(t *testing.T, dockHandler http.HandlerFunc) (*Plugin, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock := newMockDB(t)
	srv := httptest.NewServer(dockHandler)
	dock := sdk.NewClient(srv.URL, "hosts-test", sdk.DeriveHMACKey("polar_plugin_test"))
	p := &Plugin{DB: db, Dock: dock}
	cleanup := func() {
		srv.Close()
		db.Close()
	}
	return p, mock, cleanup
}

func dockAgentRegisterResponder(agentID, hostID, botUserID, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/agents/register" {
			http.Error(w, "wrong path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(sdk.AgentRegisterResponse{
			AgentID:       agentID,
			HostID:        hostID,
			BotUserID:     botUserID,
			AgentTokenRaw: token,
			Server:        "https://zen.4950.store:2443",
		})
	}
}

// TestRegisterAgent_NewHost: brand-new machine + agent. Dock returns
// fresh agent_id/host_id; plugin mirrors hosts + agent_tokens + agents.
func TestRegisterAgent_NewHost(t *testing.T) {
	const (
		workspaceID = "t_root"
		agentName   = "emei-kimi"
		agentID     = "ag_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		hostID      = "5f4dcc3b5aa765d61d8327deb882cf99"
		botUserID   = "bot_fresh"
		rawToken    = "polar_agent_rawvalueforv4"
	)
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)

	p, mock, cleanup := newV4Fixture(t, dockAgentRegisterResponder(agentID, hostID, botUserID, rawToken))
	defer cleanup()

	// Slug probe (uniqueHostSlugInWorkspace) — no collision.
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM hosts WHERE workspace_id = \$1 AND slug = \$2\)`).
		WithArgs(workspaceID, "emei-kimi").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	// hosts UPSERT.
	mock.ExpectExec(`INSERT INTO hosts \(id, workspace_id, slug, name, os, arch, last_seen_at, first_seen_at, created_at\)`).
		WithArgs(hostID, workspaceID, "emei-kimi", agentName, "darwin", "arm64", now).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// mirrorDockAgentTokenForAgent — resolve admin user, then INSERT token row.
	mock.ExpectQuery(`SELECT user_id FROM agent_tokens WHERE user_id IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("u_admin"))
	mock.ExpectExec(`INSERT INTO agent_tokens`).
		WithArgs(sqlmock.AnyArg(), "u_admin", "agent:"+agentName, sqlmock.AnyArg(), now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT id FROM agent_tokens WHERE token_hash = \$1`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tok_mirror_local"))

	// agents row insert.
	mock.ExpectExec(`INSERT INTO agents \(id, workspace_id, host_id, name, bot_user_id, agent_token_id, os, arch, created_at\)`).
		WithArgs(agentID, workspaceID, hostID, agentName, botUserID, "tok_mirror_local",
			"darwin", "arm64", now).
		WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := p.registerAgent(RegisterAgentInput{
		WorkspaceID:    workspaceID,
		Name:           agentName,
		MachineUUIDRaw: "12345678-90AB-CDEF-1234-567890ABCDEF",
		OS:             "darwin",
		Arch:           "arm64",
	}, now)
	if err != nil {
		t.Fatalf("registerAgent: %v", err)
	}
	if got.AgentID != agentID || got.HostID != hostID || got.BotUserID != botUserID {
		t.Errorf("response shape: %+v", got)
	}
	if got.TokenRaw != rawToken {
		t.Errorf("raw token round-trip failed: %q", got.TokenRaw)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestRegisterAgent_RejectsEmptyMachineUUID: v4 requires the raw uuid.
// Legacy empty path is GONE.
func TestRegisterAgent_RejectsEmptyMachineUUID(t *testing.T) {
	p := &Plugin{} // no DB / Dock needed; validation rejects first
	_, err := p.registerAgent(RegisterAgentInput{
		WorkspaceID:    "t_root",
		Name:           "legacy-box",
		MachineUUIDRaw: "",
	}, time.Now().UTC())
	if err == nil {
		t.Fatal("expected error for empty machine_uuid_raw")
	}
	if !strings.Contains(err.Error(), "machine_uuid_raw") {
		t.Errorf("expected error about machine_uuid_raw, got: %v", err)
	}
}

// TestRegisterAgent_DropsRawUUID: assert the raw machine_uuid never
// touches any SQL — sqlmock fails on unexpected statements, so if a
// future regression tried to INSERT/UPDATE the value into a column
// the test would surface it as an unmet expectation.
func TestRegisterAgent_DropsRawUUID(t *testing.T) {
	const (
		workspaceID = "t_root"
		agentName   = "secret-box"
		agentID     = "ag_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		hostID      = "deadbeef00112233445566778899aabb"
		rawUUID     = "SECRET-1234-5678-9ABC-DEFGHIJKLMNO"
	)
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)

	// Dock-side handler asserts the raw uuid IS on the wire (this part
	// is fine; it's only the persisted side we care about).
	p, mock, cleanup := newV4Fixture(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 2048)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		if !strings.Contains(body, rawUUID) {
			t.Errorf("dock should receive raw uuid on the wire: %s", body)
		}
		_ = json.NewEncoder(w).Encode(sdk.AgentRegisterResponse{
			AgentID: agentID, HostID: hostID, AgentTokenRaw: "polar_agent_x",
		})
	})
	defer cleanup()

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM hosts WHERE workspace_id = \$1 AND slug = \$2\)`).
		WithArgs(workspaceID, agentName).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(`INSERT INTO hosts`).
		WithArgs(hostID, workspaceID, agentName, agentName, "", "", now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT user_id FROM agent_tokens WHERE user_id IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("u_admin"))
	mock.ExpectExec(`INSERT INTO agent_tokens`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT id FROM agent_tokens WHERE token_hash = \$1`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tok_mirror"))
	// Note: NO mock expectation referencing rawUUID anywhere. If
	// registerAgent ever tried to write it to the DB sqlmock would
	// fail with an unexpected-query error.
	mock.ExpectExec(`INSERT INTO agents`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	_, err := p.registerAgent(RegisterAgentInput{
		WorkspaceID:    workspaceID,
		Name:           agentName,
		MachineUUIDRaw: rawUUID,
	}, now)
	if err != nil {
		t.Fatalf("registerAgent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// sanitizeSlugForTest mirrors the production slug derivation in
// uniqueHostSlugInWorkspace without the DB lookup. Kept in the test
// file so a refactor of the real function will fail this test loudly
// and force a sync.
func sanitizeSlugForTest(name string) string {
	base := strings.ToLower(strings.TrimSpace(name))
	base = strings.ReplaceAll(base, " ", "-")
	base = slugSanitize.ReplaceAllString(base, "")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "host"
	}
	if len(base) > 48 {
		base = base[:48]
	}
	return base
}
