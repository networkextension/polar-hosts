package hosts

// Tests for the v4 agents read endpoints. Pure-logic + sqlmock for the
// store helpers; HTTP wiring tested via gin recorder. See
// doc/arch/agent-identity-v4.md.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestAgentListItemMarshalShape locks the wire shape Phase F's UI
// (src/api/agents.ts + src/types/agents.ts) consumes. The
// agent_token_id_suffix MUST be present and MUST NOT be the raw token —
// see AgentListItem doc for the security rationale.
func TestAgentListItemMarshalShape(t *testing.T) {
	last := "2026-05-27T11:30:00Z"
	it := AgentListItem{
		ID:                 "ag_abcdef0123456789abcdef0123456789",
		WorkspaceID:        "t_root",
		HostID:             "5f4dcc3b5aa765d61d8327deb882cf99",
		HostName:           "emei",
		HostHWModel:        "Mac15,8",
		Name:               "emei-kimi",
		BotUserID:          "bot_xyz",
		AgentTokenIDSuffix: "abcd1234",
		OS:                 "darwin",
		Arch:               "arm64",
		CreatedAt:          "2026-05-27T10:00:00Z",
		LastHelloAt:        &last,
	}
	raw, err := json.Marshal(it)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	for _, needle := range []string{
		`"id":"ag_abcdef0123456789abcdef0123456789"`,
		`"workspace_id":"t_root"`,
		`"host_id":"5f4dcc3b5aa765d61d8327deb882cf99"`,
		`"host_name":"emei"`,
		`"host_hw_model":"Mac15,8"`,
		`"agent_token_id_suffix":"abcd1234"`,
		`"bot_user_id":"bot_xyz"`,
		`"last_hello_at":"2026-05-27T11:30:00Z"`,
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("missing %q in: %s", needle, body)
		}
	}
	// Defensive: the raw token MUST NOT appear anywhere in the row.
	// Sentinel — a future schema mistake (e.g. exposing
	// agent_token.token_hash or, worse, an unhashed token column)
	// would surface as a hex / "polar_agent_" prefix in this payload.
	for _, badPrefix := range []string{"polar_agent_", "token_hash"} {
		if strings.Contains(body, badPrefix) {
			t.Errorf("AgentListItem leaked %q-shaped content: %s", badPrefix, body)
		}
	}
}

// TestListAgents_QueryShape verifies the JOIN against hosts uses the
// expected columns + ORDER BY + ensures the suffix is computed via
// RIGHT(agent_token_id, 8) in SQL (so the raw bearer is impossible
// to leak through this code path even by a future incorrect Scan).
func TestListAgents_QueryShape(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()
	p := &Plugin{DB: db}

	created := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	lastHello := time.Date(2026, 5, 27, 11, 30, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT a.id, a.workspace_id, a.host_id,\s+COALESCE\(h.name, ''\),\s+COALESCE\(h.host_info_json->>'hw_model', ''\),\s+a.name,\s+COALESCE\(a.bot_user_id, ''\),\s+RIGHT\(a.agent_token_id, 8\),\s+COALESCE\(a.os, ''\),\s+COALESCE\(a.arch, ''\),\s+a.created_at,\s+a.last_hello_at\s+FROM agents a\s+LEFT JOIN hosts h ON h.id = a.host_id\s+WHERE a.workspace_id = \$1`).
		WithArgs("t_root").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "host_id", "host_name", "host_hw_model",
			"name", "bot_user_id", "agent_token_id_suffix",
			"os", "arch", "created_at", "last_hello_at",
		}).AddRow(
			"ag_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "t_root", "5f4dcc3b",
			"emei", "Mac15,8", "emei-kimi", "bot_xyz", "abcd1234",
			"darwin", "arm64", created, lastHello,
		))

	out, err := p.listAgents("t_root")
	if err != nil {
		t.Fatalf("listAgents: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len: %d", len(out))
	}
	got := out[0]
	if got.AgentTokenIDSuffix != "abcd1234" {
		t.Errorf("suffix: %q", got.AgentTokenIDSuffix)
	}
	if got.HostName != "emei" || got.HostHWModel != "Mac15,8" {
		t.Errorf("host join: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestGetAgent_ScanRow exercises the row-shape decode (the
// nullable last_hello_at branch in particular).
func TestGetAgent_ScanRow(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()
	p := &Plugin{DB: db}

	created := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM agents a\s+LEFT JOIN hosts h ON h.id = a.host_id\s+WHERE a.id = \$1`).
		WithArgs("ag_xyz").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "host_id", "host_name", "host_hw_model",
			"name", "bot_user_id", "agent_token_id_suffix",
			"os", "arch", "created_at", "last_hello_at",
		}).AddRow(
			"ag_xyz", "t_root", "h_abc", "emei", "",
			"emei-codex", "", "12345678",
			"linux", "amd64", created, nil, // last_hello NULL
		))

	got, err := p.getAgent("ag_xyz")
	if err != nil {
		t.Fatalf("getAgent: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil agent")
	}
	if got.LastHelloAt != nil {
		t.Errorf("expected nil last_hello_at, got %v", got.LastHelloAt)
	}
	if got.BotUserID != "" {
		t.Errorf("empty bot_user_id expected, got %q", got.BotUserID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestHostMarshalShape_V4Fields locks the v4 additions to Host JSON:
// mem_peak_bytes + cpu_peak_pct + agents_count must all surface in the
// /api/hosts payload so the Phase F UI's peak-badge + "▾ 实例 (N)"
// footer render correctly.
func TestHostMarshalShape_V4Fields(t *testing.T) {
	memPeak := int64(32212254720)
	cpuPeak := 92.4
	h := Host{
		ID:           "5f4dcc3b5aa765d61d8327deb882cf99",
		WorkspaceID:  "t_root",
		Slug:         "emei",
		Name:         "emei",
		OS:           "darwin",
		Arch:         "arm64",
		MemPeakBytes: &memPeak,
		CPUPeakPct:   &cpuPeak,
		AgentsCount:  3,
	}
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	for _, needle := range []string{
		`"mem_peak_bytes":32212254720`,
		`"cpu_peak_pct":92.4`,
		`"agents_count":3`,
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("missing %q in: %s", needle, body)
		}
	}
}

// TestHostMarshalShape_V4Fields_OmittedWhenNull: nullable cols should
// drop from the JSON when the agent hasn't sampled yet — the UI's
// peak-color logic distinguishes "no data" from "0".
func TestHostMarshalShape_V4Fields_OmittedWhenNull(t *testing.T) {
	h := Host{
		ID:          "h_abc",
		WorkspaceID: "t_root",
		Slug:        "fresh",
		Name:        "fresh",
		AgentsCount: 0,
	}
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	if strings.Contains(body, "mem_peak_bytes") {
		t.Errorf("mem_peak_bytes should be omitted when nil: %s", body)
	}
	if strings.Contains(body, "cpu_peak_pct") {
		t.Errorf("cpu_peak_pct should be omitted when nil: %s", body)
	}
	// agents_count is always present (no omitempty) so the UI can rely
	// on a numeric 0 vs missing field for an "n/a" indicator.
	if !strings.Contains(body, `"agents_count":0`) {
		t.Errorf("agents_count should always serialize: %s", body)
	}
}
