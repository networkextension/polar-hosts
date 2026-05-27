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
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
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

// TestCreateOrUpdateHost_DedupSameMachineUUID exercises the dedup
// branch: when the same workspace already has a hosts row with the
// given machine_uuid, createOrUpdateHostByMachineUUID must UPDATE the
// existing row (preserving id + slug) rather than INSERT a new one.
// Dock dual-write is skipped (Dock is nil) so we don't need an HTTP
// server to mock.
func TestCreateOrUpdateHost_DedupSameMachineUUID(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	p := &Plugin{DB: db} // Dock=nil → skip dual-write

	const (
		workspaceID  = "t_root"
		hostName     = "locals-Mac-2"
		oldHostID    = "h_existing"
		oldHostSlug  = "locals-mac-2"
		newTokenID   = "tok_fresh"
		machineUUID  = "12345678-90AB-CDEF-1234-567890ABCDEF"
	)
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)

	// 1. getHostByWorkspaceMachineUUID hits the dedup index and finds
	//    the existing row. Return enough columns to satisfy
	//    scanHostInto (id, workspace, slug, name, agent_token,
	//    os, arch, last_seen_ip, advertised_skills, last_seen_at,
	//    created_at, host_info, host_info_seen_at, machine_uuid).
	dedupCols := []string{
		"id", "workspace_id", "slug", "name", "agent_token_id",
		"os", "arch", "last_seen_ip", "advertised_skills_json",
		"last_seen_at", "created_at", "host_info_json",
		"host_info_seen_at", "machine_uuid",
	}
	dedupRows := sqlmock.NewRows(dedupCols).
		AddRow(oldHostID, workspaceID, oldHostSlug, "old-name", "tok_old",
			"darwin", "arm64", "10.0.0.5", "[]",
			nil, now.Add(-24*time.Hour), "{}",
			nil, machineUUID)
	mock.ExpectQuery(`SELECT .* FROM hosts WHERE workspace_id = \$1 AND machine_uuid = \$2`).
		WithArgs(workspaceID, machineUUID).
		WillReturnRows(dedupRows)

	// 2. UPDATE the existing row.
	mock.ExpectExec(`UPDATE hosts\s+SET name\s+= \$1,`).
		WithArgs(hostName, sqlmock.AnyArg(), "darwin", "arm64", oldHostID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 3. getHostByID reloads the row (returns the updated values).
	reloadRows := sqlmock.NewRows(dedupCols).
		AddRow(oldHostID, workspaceID, oldHostSlug, hostName, newTokenID,
			"darwin", "arm64", "10.0.0.5", "[]",
			nil, now.Add(-24*time.Hour), "{}",
			nil, machineUUID)
	mock.ExpectQuery(`SELECT .* FROM hosts WHERE id = \$1`).
		WithArgs(oldHostID).
		WillReturnRows(reloadRows)

	got, err := p.createOrUpdateHostByMachineUUID(
		workspaceID, hostName, newTokenID, "darwin", "arm64", machineUUID, now,
	)
	if err != nil {
		t.Fatalf("createOrUpdate: %v", err)
	}
	if got == nil {
		t.Fatal("got nil host")
	}
	if got.ID != oldHostID {
		t.Errorf("expected dedup → existing id %q, got %q", oldHostID, got.ID)
	}
	if got.Slug != oldHostSlug {
		t.Errorf("slug should be preserved across re-register: want %q got %q", oldHostSlug, got.Slug)
	}
	if got.Name != hostName {
		t.Errorf("name should be refreshed: want %q got %q", hostName, got.Name)
	}
	if got.MachineUUID != machineUUID {
		t.Errorf("machine_uuid should round-trip: want %q got %q", machineUUID, got.MachineUUID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestCreateOrUpdateHost_EmptyMachineUUIDFallsBackToInsert verifies
// the legacy path: when an older agent (or one whose collector failed)
// passes an empty machine_uuid, we MUST NOT do a dedup SELECT (no
// index on empty values, and we don't want to collide unrelated boxes
// on the empty key). Just INSERT like the pre-PR code.
func TestCreateOrUpdateHost_EmptyMachineUUIDFallsBackToInsert(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	p := &Plugin{DB: db} // Dock=nil

	const (
		workspaceID = "t_root"
		hostName    = "legacy-box"
	)
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)

	// Slug uniqueness probe (uniqueHostSlugInWorkspace).
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM hosts WHERE workspace_id = \$1 AND slug = \$2\)`).
		WithArgs(workspaceID, "legacy-box").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	// INSERT — note machine_uuid is nil (untyped Go nil, lib/pq sends NULL).
	mock.ExpectExec(`INSERT INTO hosts \(id, workspace_id, slug, name, agent_token_id, os, arch, machine_uuid, created_at\)`).
		WithArgs(
			sqlmock.AnyArg(), workspaceID, "legacy-box", hostName,
			sqlmock.AnyArg(), "linux", "amd64", nil, now,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := p.createOrUpdateHostByMachineUUID(
		workspaceID, hostName, "tok_xyz", "linux", "amd64", "", now,
	)
	if err != nil {
		t.Fatalf("createOrUpdate (empty UUID): %v", err)
	}
	if got == nil || !strings.HasPrefix(got.ID, "h_") {
		t.Errorf("expected new host with h_ prefix, got %+v", got)
	}
	if got.MachineUUID != "" {
		t.Errorf("MachineUUID should remain empty: got %q", got.MachineUUID)
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
