package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

type node struct {
	zone, path, resolver, token string
	mu                          sync.Mutex
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8443", "management HTTPS address")
	zone := flag.String("zone", "", "DNS zone name")
	path := flag.String("file", "", "local zone file used by CoreDNS")
	resolver := flag.String("dns", "127.0.0.1:53", "local CoreDNS address")
	cert := flag.String("tls-cert", "", "TLS certificate file")
	key := flag.String("tls-key", "", "TLS key file")
	clientCA := flag.String("client-ca", "", "CA PEM for authenticating the controller")
	flag.Parse()
	token := os.Getenv("DNS_NODE_TOKEN")
	if *zone == "" || *path == "" || *cert == "" || *key == "" || len(token) < 32 {
		log.Fatal("zone, file, TLS certificate, TLS key and DNS_NODE_TOKEN (at least 32 bytes) are required")
	}
	host, _, err := net.SplitHostPort(*resolver)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("dns must be a loopback IP and port")
	}
	n := &node{dns.CanonicalName(*zone), *path, *resolver, token, sync.Mutex{}}
	if err := n.recover(); err != nil {
		log.Fatalf("recover interrupted publication: %v", err)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if *clientCA != "" {
		pem, err := os.ReadFile(*clientCA)
		if err != nil {
			log.Fatal(err)
		}
		tlsConfig.ClientCAs = x509.NewCertPool()
		if !tlsConfig.ClientCAs.AppendCertsFromPEM(pem) {
			log.Fatal("invalid controller CA")
		}
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}
	server := &http.Server{Addr: *listen, Handler: http.HandlerFunc(n.serve),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 45 * time.Second,
		TLSConfig: tlsConfig}
	log.Fatal(server.ListenAndServeTLS(*cert, *key))
}

func (n *node) serve(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+n.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path != "/v1/zone" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var data []byte
	if r.Method == http.MethodPut {
		var err error
		data, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "cannot read zone file", http.StatusBadRequest)
			return
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	var serial uint32
	var err error
	if r.Method == http.MethodGet {
		serial, err = n.query(r.Context())
	} else {
		serial, err = n.publish(data)
	}
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]uint32{"serial": serial})
}

func (n *node) serial(data []byte) (uint32, error) {
	parser := dns.NewZoneParser(bytes.NewReader(data), n.zone, n.path)
	var soa *dns.SOA
	for rr, ok := parser.Next(); ok; rr, ok = parser.Next() {
		if !dns.IsSubDomain(n.zone, dns.CanonicalName(rr.Header().Name)) || rr.Header().Class != dns.ClassINET {
			return 0, errors.New("record outside zone or not in IN class")
		}
		if value, ok := rr.(*dns.SOA); ok {
			if soa != nil || !strings.EqualFold(value.Hdr.Name, n.zone) {
				return 0, errors.New("expected one SOA at the zone apex")
			}
			soa = value
		}
	}
	if err := parser.Err(); err != nil {
		return 0, err
	}
	if soa == nil {
		return 0, errors.New("missing zone SOA")
	}
	return soa.Serial, nil
}

func (n *node) query(ctx context.Context) (uint32, error) {
	msg := new(dns.Msg).SetQuestion(n.zone, dns.TypeSOA)
	msg.RecursionDesired = false
	client := dns.Client{Timeout: time.Second}
	answer, _, err := client.ExchangeContext(ctx, msg, n.resolver)
	if err != nil {
		return 0, err
	}
	if answer.Authoritative && answer.Rcode == dns.RcodeSuccess {
		for _, rr := range answer.Answer {
			if soa, ok := rr.(*dns.SOA); ok && strings.EqualFold(soa.Hdr.Name, n.zone) {
				return soa.Serial, nil
			}
		}
	}
	return 0, errors.New("local DNS did not return an authoritative SOA")
}

func (n *node) wait(serial uint32) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if active, err := n.query(ctx); err == nil && active == serial {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("local DNS did not activate SOA serial %d", serial)
		case <-ticker.C:
		}
	}
}

func (n *node) publish(data []byte) (uint32, error) {
	next, err := n.serial(data)
	if err != nil {
		return 0, err
	}
	if err := n.recover(); err != nil {
		return 0, err
	}
	old, err := os.ReadFile(n.path)
	if err != nil {
		return 0, err
	}
	previous, err := n.serial(old)
	if err != nil {
		return 0, err
	}
	if bytes.Equal(data, old) {
		return next, n.wait(next)
	}
	if delta := next - previous; delta == 0 || delta >= 1<<31 {
		return 0, fmt.Errorf("SOA serial must advance from %d", previous)
	}
	if err = atomicWrite(n.path+".pending", old); err != nil {
		return 0, err
	}
	if err = atomicWrite(n.path, data); err == nil {
		err = n.wait(next)
	}
	if err != nil {
		return 0, errors.Join(err, n.recover())
	}
	return next, removePending(n.path + ".pending")
}

func (n *node) recover() error {
	pending := n.path + ".pending"
	old, err := os.ReadFile(pending)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	serial, err := n.serial(old)
	if err != nil {
		return err
	}
	if err := atomicWrite(n.path, old); err != nil {
		return err
	}
	if err := n.wait(serial); err != nil {
		return err
	}
	return removePending(pending)
}

func removePending(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDir(path)
}

func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".zone-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0640); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(path)
}

func syncDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
