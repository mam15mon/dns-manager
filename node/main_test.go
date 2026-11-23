package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/miekg/dns"
)

func TestRecoverInterruptedPublication(t *testing.T) {
	const zone = "infra.example.com."
	contents := func(serial int) []byte {
		return []byte(fmt.Sprintf("@ 60 IN SOA ns.%s hostmaster.%s %d 60 30 86400 60\n", zone, zone, serial))
	}

	path := filepath.Join(t.TempDir(), "zone.db")
	if err := os.WriteFile(path, contents(2), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".pending", contents(1), 0640); err != nil {
		t.Fatal(err)
	}

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	n := &node{zone: zone, path: path, resolver: conn.LocalAddr().String()}
	server := &dns.Server{PacketConn: conn, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		reply := new(dns.Msg).SetReply(r)
		reply.Authoritative = true
		data, err := os.ReadFile(path)
		if err == nil {
			serial, err := n.serial(data)
			if err == nil {
				reply.Answer = []dns.RR{&dns.SOA{
					Hdr: dns.RR_Header{Name: zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET},
					Ns:  "ns." + zone, Mbox: "hostmaster." + zone, Serial: serial,
				}}
			}
		}
		_ = w.WriteMsg(reply)
	})}
	ready := make(chan struct{})
	server.NotifyStartedFunc = func() { close(ready) }
	go func() { _ = server.ActivateAndServe() }()
	<-ready
	t.Cleanup(func() { _ = server.Shutdown() })

	if err := n.recover(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(contents(1)) {
		t.Fatalf("expected previous zone file, got %q, %v", data, err)
	}
	if _, err := os.Stat(path + ".pending"); !os.IsNotExist(err) {
		t.Fatalf("expected recovery file to be removed, got %v", err)
	}
}
