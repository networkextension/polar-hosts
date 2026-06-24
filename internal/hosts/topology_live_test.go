package hosts

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"

	_ "github.com/lib/pq"
)

// TestLiveTopology is a manual integration probe: set POLAR_TOPO_LIVE_DSN to a
// polar_hosts DSN and it builds + prints the topology for the workspace with
// the most hosts. Skipped in normal CI (no DSN). NOT a CI gate — a deploy probe.
func TestLiveTopology(t *testing.T) {
	dsn := os.Getenv("POLAR_TOPO_LIVE_DSN")
	if dsn == "" {
		t.Skip("set POLAR_TOPO_LIVE_DSN to run the live topology probe")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := &Plugin{DB: db}

	rows, err := db.Query(`SELECT workspace_id, count(*) c FROM hosts GROUP BY workspace_id ORDER BY c DESC`)
	if err != nil {
		t.Fatal(err)
	}
	type wc struct {
		ws string
		c  int
	}
	var wss []wc
	for rows.Next() {
		var w wc
		_ = rows.Scan(&w.ws, &w.c)
		wss = append(wss, w)
	}
	rows.Close()
	sort.Slice(wss, func(i, j int) bool { return wss[i].c > wss[j].c })
	if len(wss) == 0 {
		t.Fatal("no hosts")
	}
	ws := wss[0].ws
	fmt.Printf("\n=== workspace %s (%d hosts) ===\n", ws, wss[0].c)
	topo, err := p.buildTopology(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range topo.Networks {
		fmt.Printf("\n[%s] %s  center=%q(%s)  nodes=%d  anomalies=%d\n",
			n.Kind, n.Label, n.Center.Label, n.Center.Detail, len(n.Nodes), len(n.Anomalies))
		for _, nd := range n.Nodes {
			tag := ""
			if nd.IsHub {
				tag += " ★HUB"
			}
			if nd.Conflict {
				tag += " ⚠CONFLICT"
			}
			ifs, _ := json.Marshal(nd.Ifaces)
			fmt.Printf("   • %-22s online=%-5v%s  %s\n", nd.Name, nd.Online, tag, string(ifs))
		}
		for _, a := range n.Anomalies {
			fmt.Printf("   ⚠ %s: %s\n", a.Type, a.Detail)
		}
	}
}
