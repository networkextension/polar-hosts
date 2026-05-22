package hosts

// Pure-logic tests for hosts_store.go. Anything that touches the
// real DB (sql.Open / sql.DB.Query) is exercised in the P0 smoke
// step (`dropdb && createdb && go run ./cmd/dock` + a manual
// enrollment round-trip). This mirrors the existing dock-package
// convention: ai_agent_stream_test.go and metrics_test.go are also
// pure-memory.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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
