package hosts

// Host network topology builder. Parses every host's agent-pushed
// host_info_json into three networks the UI renders as tabs, each a star
// around a central 交换机/hub:
//
//   - thunderbolt — the Thunderbolt fabric (bridge0 + its en1/en2/en3 member
//                   ports). These NICs often have no IP (provisioned but not
//                   cabled), so they're keyed by MAC + iface_kind, not IP.
//   - lan         — the home LAN (Wi-Fi + wired collapsed into one), centered
//                   on the default gateway.
//   - wg          — the WireGuard overlay (10.88.0.0/24), centered on the hub.
//
// Kind tagging is authoritative from the agent for the *physical* kinds
// (thunderbolt/wifi/ethernet — only the agent can see a Thunderbolt bridge that
// has no IP). wg vs mesh vs lan is refined *here* by CIDR, because the overlay
// subnet (10.88.0.0/24) and the CGNAT mesh range (100.64.0.0/10) are
// server-side constants the agent shouldn't hardcode.
//
// See doc/arch/host-network-topology.md.

import (
	"net"
	"sort"
	"strings"
	"time"
)

// topologyOnlineWindow: a host whose last_seen_at is within this window renders
// online. Generous vs the agent hello cadence so a healthy box that heartbeats
// lazily still lights up.
const topologyOnlineWindow = 3 * time.Minute

// overlayNet / cgnatNet are the Polar-wide overlay + tailscale-style mesh
// ranges, used to reclassify a wg/utun interface by the address it carries.
var (
	overlayNet = mustCIDR("10.88.0.0/24")
	cgnatNet   = mustCIDR("100.64.0.0/10")
)

// classifyIface decides which network tab a NIC belongs to, given its
// agent-reported kind and its IPv4 (bare, may be ""). The CIDR is authoritative
// for the overlay/mesh ranges (server-side constants); the physical kind is
// authoritative for thunderbolt (which often has no IP at all). Returns:
//   "wg" | "tb" | "lan" | ""  (""=not one of the three tabs: mesh, idle port, public)
func classifyIface(kind, ip string) string {
	parsed := net.ParseIP(ip)
	switch {
	case parsed != nil && overlayNet.Contains(parsed):
		return "wg"
	case parsed != nil && cgnatNet.Contains(parsed):
		return "" // tailscale-style mesh — not one of our three tabs
	case kind == "wg":
		return "wg" // wg iface with no/odd address (userspace utun)
	case kind == "thunderbolt":
		return "tb"
	case kind == "wifi" || kind == "ethernet":
		if parsed != nil && parsed.IsPrivate() {
			return "lan"
		}
		// wifi/ethernet with no IP (idle port) or a public IP → not LAN.
		return ""
	case kind == "" && parsed != nil && parsed.IsPrivate():
		// Old-agent fallback (pre-0.4, no iface_kind): any private IPv4 that
		// isn't the overlay/mesh is the host's LAN. Lets boxes still on the
		// old agent appear in the LAN tab before the fleet rolls to 0.4.
		return "lan"
	default:
		return ""
	}
}

// isVirtualIface reports whether an interface name is an obvious container /
// VM / hypervisor NAT bridge rather than a real network NIC — used only for the
// old-agent fallback, where we lack iface_kind to tell them apart. Conservative:
// keeps en*/eth*/bridge0 (real or Internet-Sharing) and only drops the
// unmistakable virtual prefixes.
func isVirtualIface(name string) bool {
	for _, p := range []string{"docker", "vmenet", "vnic", "veth", "cni", "flannel", "tap"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	// VM bridges are bridge100/bridge101/… (bridge + multi-digit); bridge0 is
	// the Thunderbolt / Internet-Sharing bridge and is kept.
	if strings.HasPrefix(name, "bridge") {
		rest := name[len("bridge"):]
		if len(rest) >= 2 { // 2+ digits = VM bridge
			return true
		}
	}
	return false
}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// Topology is the GET /api/hosts/topology response.
type Topology struct {
	Networks    []TopoNet `json:"networks"`
	HostCount   int       `json:"host_count"`
	GeneratedAt string    `json:"generated_at"`
}

// TopoNet is one network tab: a central switch/hub + the host nodes on it.
type TopoNet struct {
	Kind      string        `json:"kind"`  // "thunderbolt" | "lan" | "wg"
	Label     string        `json:"label"` // display name for the tab
	Center    TopoCenter    `json:"center"`
	Nodes     []TopoNode    `json:"nodes"`
	Anomalies []TopoAnomaly `json:"anomalies,omitempty"`
}

// TopoCenter is the 中央交换机 / hub drawn in the middle of the star.
type TopoCenter struct {
	Shape  string `json:"shape"` // "switch" (TB bridge / LAN switch) | "hub" (wg)
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

// TopoNode is one host on a network (a spoke). A host appears in every network
// it has a NIC on (the tabs intentionally isolate networks).
type TopoNode struct {
	HostID   string      `json:"host_id"`
	Name     string      `json:"name"`
	Online   bool        `json:"online"`
	IsHub    bool        `json:"is_hub,omitempty"`   // this host IS the wg hub (10.88.0.1)
	Conflict bool        `json:"conflict,omitempty"` // shares an IP with another host
	Ifaces   []TopoIface `json:"ifaces"`
}

// TopoIface is one NIC the host has on this network.
type TopoIface struct {
	Iface string `json:"iface"`
	IP    string `json:"ip,omitempty"`
	MAC   string `json:"mac,omitempty"`
	Kind  string `json:"kind"`
	Stale bool   `json:"stale,omitempty"` // un-cabled TB port / link with no address
}

// TopoAnomaly flags a problem the UI highlights (currently IP conflicts).
type TopoAnomaly struct {
	Type   string   `json:"type"`
	Detail string   `json:"detail"`
	Hosts  []string `json:"hosts,omitempty"`
}

// buildTopology assembles the three-network topology for a workspace from the
// hosts' host_info blobs. Pure read; cheap enough to cache 15s at the handler.
func (p *Plugin) buildTopology(workspaceID string) (*Topology, error) {
	hosts, err := p.listHostsForWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	now := time.Now()

	tb := newBucket()
	lan := newBucket()
	wg := newBucket()

	gwVotes := map[string]int{} // default_gw → count (LAN switch label)
	// ipHosts[bucket][ip] → host names sharing it (conflict detection on the
	// IP-bearing networks: lan + wg).
	ipHosts := map[string]map[string][]string{"lan": {}, "wg": {}}
	wgHubIP := ""

	for _, h := range hosts {
		hi := h.HostInfo
		kinds := stringMap(hi, "iface_kind")
		macs := stringMap(hi, "mac_by_iface")
		cidrs := stringMap(hi, "ipv4_cidr_by_iface")
		if len(cidrs) == 0 {
			// Old agent (pre-0.4): no CIDR map — fall back to the bare-IP map.
			cidrs = stringMap(hi, "ipv4_by_iface")
		}
		if gw := hiString(hi, "default_gw"); gw != "" {
			gwVotes[gw]++
		}
		online := h.LastSeenAt != nil && now.Sub(*h.LastSeenAt) < topologyOnlineWindow

		for _, ifc := range unionKeys(kinds, cidrs, macs) {
			kind := kinds[ifc]
			ip := stripPrefix(cidrs[ifc])
			mac := macs[ifc]
			// Old-agent fallback (kind=="") can't tell a real NIC from a
			// container/VM bridge; drop the obvious virtual ones so the LAN tab
			// isn't polluted by docker0 / VM NAT bridges. (0.4 agents tag these
			// "other" via iface_kind and never reach here.)
			if kind == "" && isVirtualIface(ifc) {
				continue
			}
			bucket := classifyIface(kind, ip)
			if bucket == "" {
				continue
			}

			iface := TopoIface{Iface: ifc, IP: ip, MAC: mac, Kind: kind, Stale: ip == ""}
			switch bucket {
			case "tb":
				tb.add(h, online).addIface(iface)
			case "lan":
				lan.add(h, online).addIface(iface)
				if ip != "" {
					ipHosts["lan"][ip] = appendUnique(ipHosts["lan"][ip], h.Name)
				}
			case "wg":
				n := wg.add(h, online)
				n.addIface(iface)
				if ip != "" {
					ipHosts["wg"][ip] = appendUnique(ipHosts["wg"][ip], h.Name)
				}
				if ip == "10.88.0.1" {
					n.node.IsHub = true
					wgHubIP = ip
				}
			}
		}
	}

	// IP conflicts → anomalies + mark the offending nodes, per IP-bearing net.
	lanAnomalies := flagConflicts(lan, ipHosts["lan"])
	wgAnomalies := flagConflicts(wg, ipHosts["wg"])

	gateway := topVote(gwVotes)
	if wgHubIP == "" {
		wgHubIP = "10.88.0.1"
	}

	nets := []TopoNet{
		{
			Kind:   "thunderbolt",
			Label:  "⚡ 雷电",
			Center: TopoCenter{Shape: "switch", Label: "雷电交换机", Detail: "Thunderbolt Bridge · 40 Gb/s"},
			Nodes:  tb.sorted(),
		},
		{
			Kind:      "lan",
			Label:     "🏠 局域网",
			Center:    TopoCenter{Shape: "switch", Label: "局域网交换机", Detail: gateway},
			Nodes:     lan.sorted(),
			Anomalies: lanAnomalies,
		},
		{
			Kind:      "wg",
			Label:     "🔒 WG Link",
			Center:    TopoCenter{Shape: "hub", Label: "WG Hub", Detail: wgHubIP + " · 10.88.0.0/24"},
			Nodes:     wg.sorted(),
			Anomalies: wgAnomalies,
		},
	}

	return &Topology{
		Networks:    nets,
		HostCount:   len(hosts),
		GeneratedAt: now.UTC().Format(time.RFC3339),
	}, nil
}

// flagConflicts finds IPs shared by >1 host within a bucket, marks each
// offending node Conflict, and returns one anomaly per conflicting IP. Mutates
// the bucket's nodes in place.
func flagConflicts(b *nodeBucket, ipHosts map[string][]string) []TopoAnomaly {
	conflictIPs := map[string]bool{}
	var anomalies []TopoAnomaly
	// Deterministic anomaly order: sort the conflicting IPs.
	var ips []string
	for ip, names := range ipHosts {
		if len(names) > 1 {
			ips = append(ips, ip)
		}
	}
	sort.Strings(ips)
	for _, ip := range ips {
		conflictIPs[ip] = true
		names := append([]string(nil), ipHosts[ip]...)
		sort.Strings(names)
		anomalies = append(anomalies, TopoAnomaly{
			Type:   "ip_conflict",
			Detail: ip + " 被 " + strings.Join(names, " / ") + " 同时占用",
			Hosts:  names,
		})
	}
	for _, id := range b.order {
		for _, ifc := range b.nodes[id].Ifaces {
			if conflictIPs[ifc.IP] {
				b.nodes[id].Conflict = true
			}
		}
	}
	return anomalies
}

// --- small builder helpers ------------------------------------------------

// nodeBucket accumulates one network's nodes keyed by host id, preserving
// first-seen order so the render is deterministic.
type nodeBucket struct {
	nodes map[string]*TopoNode
	order []string
}

func newBucket() *nodeBucket { return &nodeBucket{nodes: map[string]*TopoNode{}} }

// nodeRef is a thin handle so callers can chain .addIface().
type nodeRef struct{ node *TopoNode }

func (b *nodeBucket) add(h Host, online bool) nodeRef {
	n, ok := b.nodes[h.ID]
	if !ok {
		n = &TopoNode{HostID: h.ID, Name: h.Name, Online: online}
		b.nodes[h.ID] = n
		b.order = append(b.order, h.ID)
	}
	return nodeRef{n}
}

func (r nodeRef) addIface(i TopoIface) { r.node.Ifaces = append(r.node.Ifaces, i) }

// sorted returns the bucket's nodes online-first then by name — a stable,
// readable spoke order.
func (b *nodeBucket) sorted() []TopoNode {
	out := make([]TopoNode, 0, len(b.order))
	for _, id := range b.order {
		out = append(out, *b.nodes[id])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsHub != out[j].IsHub {
			return out[i].IsHub // hub first
		}
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// --- host_info parsing helpers (the blob is map[string]any from JSON) -----

func stringMap(hi map[string]any, key string) map[string]string {
	out := map[string]string{}
	raw, ok := hi[key].(map[string]any)
	if !ok {
		return out
	}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func hiString(hi map[string]any, key string) string {
	s, _ := hi[key].(string)
	return s
}

// stripPrefix turns "192.168.11.57/24" → "192.168.11.57" (and leaves a bare IP
// untouched). Returns "" for empty.
func stripPrefix(s string) string {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i]
	}
	return s
}

// unionKeys returns the sorted union of all keys across the given maps — the
// set of interfaces the host has any data for.
func unionKeys(maps ...map[string]string) []string {
	seen := map[string]bool{}
	for _, m := range maps {
		for k := range m {
			seen[k] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func appendUnique(xs []string, x string) []string {
	for _, e := range xs {
		if e == x {
			return xs
		}
	}
	return append(xs, x)
}

// topVote returns the most-common key (the LAN's shared gateway). Empty when no
// votes. Deterministic tie-break by string for stable output.
func topVote(votes map[string]int) string {
	best, bestN := "", 0
	for k, n := range votes {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}
