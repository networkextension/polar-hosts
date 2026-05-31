package hosts

import "testing"

// TestResolveWorkspaceID covers the workspace-scoping decision that fixes
// the "hosts invisible" bug: requests scope to the SELECTED workspace
// (X-Workspace-Id) when the user is a member, else the personal default —
// never to a foreign workspace.
func TestResolveWorkspaceID(t *testing.T) {
	const personal = "ws_personal"
	const root = "ws_root"
	const foreign = "ws_other_tenant"
	const uid = "u1"

	memberOf := func(allowed ...string) func(string, string) bool {
		set := map[string]bool{}
		for _, a := range allowed {
			set[a] = true
		}
		return func(teamID, userID string) bool { return userID == uid && set[teamID] }
	}

	cases := []struct {
		name      string
		requested string
		isMember  func(string, string) bool
		want      string
	}{
		{"no header → personal", "", memberOf(root), personal},
		{"requested == personal → personal", personal, memberOf(root), personal},
		{"member of requested → requested", root, memberOf(root), root},
		{"NOT a member → personal (no leak)", foreign, memberOf(root), personal},
		{"whitespace header → personal", "   ", memberOf(root), personal},
		{"nil isMember → personal", root, nil, personal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveWorkspaceID(tc.requested, personal, uid, tc.isMember)
			if got != tc.want {
				t.Fatalf("resolveWorkspaceID(%q) = %q, want %q", tc.requested, got, tc.want)
			}
		})
	}
}
