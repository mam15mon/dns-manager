package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestZoneFile(t *testing.T) {
	forward := zone{Name: "infra.example.com.", Nameserver: "ns.infra.example.com.", Contact: "hostmaster.infra.example.com."}
	reverse := zone{Name: "2.0.192.in-addr.arpa.", Nameserver: forward.Nameserver, Contact: forward.Contact}
	records := []record{
		{Name: "git", Type: "A", Value: "192.0.2.20", TTL: 60},
		{Name: "git", Type: "AAAA", Value: "2001:db8::20", TTL: 60},
		{Name: "code", Type: "CNAME", Value: "git.infra.example.com", TTL: 60},
	}
	file, err := zoneFile(forward, 2, records)
	if err != nil || !strings.Contains(file, "git.infra.example.com.") {
		t.Fatalf("forward zone = %q, %v", file, err)
	}
	if _, err := zoneFile(reverse, 2, []record{{Name: "20", Type: "PTR", Value: "git.infra.example.com.", TTL: 60}}); err != nil {
		t.Fatal(err)
	}
	if _, err := zoneFile(forward, 2, append(records, record{Name: "code", Type: "A", Value: "192.0.2.21", TTL: 60})); err == nil {
		t.Fatal("accepted an A record at a CNAME owner")
	}
}

func TestReleaseHistoryAndRollback(t *testing.T) {
	z := zone{Name: "infra.example.com.", Nameserver: "ns.infra.example.com.", Contact: "hostmaster.infra.example.com.", InitialSerial: 5,
		InitialRecords: []record{{Name: "git", Type: "A", Value: "192.0.2.20", TTL: 60}}}
	c := &controller{path: filepath.Join(t.TempDir(), "releases.json"), zones: map[string]zone{z.Name: z}, history: make(map[string][]release)}
	records := []record{{Name: "git", Type: "A", Value: "192.0.2.21", TTL: 60}}
	if serial, err := c.publish(z.Name, records, 0); err != nil || serial != 6 {
		t.Fatalf("published serial %d: %v", serial, err)
	}
	if serial, err := c.publish(z.Name, nil, 5); err != nil || serial != 7 {
		t.Fatalf("rollback serial %d: %v", serial, err)
	}
	var persisted map[string][]release
	if err := readJSON(c.path, &persisted); err != nil {
		t.Fatal(err)
	}
	if got := persisted[z.Name][1].Records[0].Value; got != "192.0.2.20" {
		t.Fatalf("rollback restored %s", got)
	}
}
