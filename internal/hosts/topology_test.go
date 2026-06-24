package hosts

import "testing"

// helpers operating on the parsed-blob shape (map[string]any, like JSON).

func TestStripPrefix(t *testing.T) {
	cases := map[string]string{
		"192.168.11.57/24": "192.168.11.57",
		"10.88.0.1/8":      "10.88.0.1",
		"192.168.11.65":    "192.168.11.65",
		"":                 "",
	}
	for in, want := range cases {
		if got := stripPrefix(in); got != want {
			t.Errorf("stripPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStringMapAndUnionKeys(t *testing.T) {
	hi := map[string]any{
		"iface_kind": map[string]any{"bridge0": "thunderbolt", "en0": "wifi"},
		"mac_by_iface": map[string]any{"bridge0": "36:83:9a:11:75:80", "en1": "36:83:9a:11:75:81"},
	}
	kinds := stringMap(hi, "iface_kind")
	if kinds["bridge0"] != "thunderbolt" || kinds["en0"] != "wifi" {
		t.Fatalf("kinds = %v", kinds)
	}
	if got := stringMap(hi, "missing"); len(got) != 0 {
		t.Errorf("missing key should give empty map, got %v", got)
	}
	macs := stringMap(hi, "mac_by_iface")
	keys := unionKeys(kinds, macs)
	// union of {bridge0,en0} ∪ {bridge0,en1} = bridge0,en0,en1 (sorted)
	want := []string{"bridge0", "en0", "en1"}
	if len(keys) != len(want) {
		t.Fatalf("union = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("union[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
}

func TestTopVote(t *testing.T) {
	if got := topVote(map[string]int{"192.168.11.1": 5, "192.168.2.1": 2}); got != "192.168.11.1" {
		t.Errorf("topVote = %q, want 192.168.11.1", got)
	}
	if got := topVote(map[string]int{}); got != "" {
		t.Errorf("empty topVote = %q, want empty", got)
	}
}

// buildTopologyFromHosts mirrors buildTopology's per-host classification but
// takes hosts directly (no DB) so the bucketing logic is unit-testable. Keep in
// sync with buildTopology — this asserts the network assignment rules.
func TestNetworkClassification(t *testing.T) {
	// A zen-shaped host: en10 wired LAN, en0 wifi LAN, bridge0+en1 thunderbolt
	// (no IP), utun0 carrying the wg overlay address (kind reported as mesh).
	hi := map[string]any{
		"iface_kind": map[string]any{
			"en10": "ethernet", "en0": "wifi",
			"bridge0": "thunderbolt", "en1": "thunderbolt",
			"utun0": "mesh", "utun5": "mesh",
		},
		"ipv4_cidr_by_iface": map[string]any{
			"en10": "192.168.11.57/24", "en0": "192.168.11.65/24",
			"utun0": "10.88.0.1/8",       // overlay → wg (refined by CIDR)
			"utun5": "100.64.0.3/10",     // CGNAT → mesh → dropped
		},
		"mac_by_iface": map[string]any{
			"bridge0": "36:83:9a:11:75:80", "en1": "36:83:9a:11:75:81",
		},
		"default_gw": "192.168.11.1",
	}

	kinds := stringMap(hi, "iface_kind")
	cidrs := stringMap(hi, "ipv4_cidr_by_iface")
	got := map[string]string{} // iface → bucket
	for _, ifc := range unionKeys(kinds, cidrs, stringMap(hi, "mac_by_iface")) {
		got[ifc] = classifyIface(kinds[ifc], stripPrefix(cidrs[ifc]))
	}

	want := map[string]string{
		"en10":    "lan",
		"en0":     "lan",
		"bridge0": "tb",
		"en1":     "tb",
		"utun0":   "wg",  // overlay IP wins over its "mesh" kind tag
		"utun5":   "",    // CGNAT → mesh → not one of the three tabs
	}
	for ifc, w := range want {
		if got[ifc] != w {
			t.Errorf("iface %s → bucket %q, want %q", ifc, got[ifc], w)
		}
	}
}
