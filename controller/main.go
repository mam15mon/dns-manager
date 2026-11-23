package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

//go:embed index.html
var index []byte

type target struct {
	Name, Region, URL, TokenEnv string
	token                       string
}

type zone struct {
	Name, Nameserver, Contact string
	InitialSerial             uint32
	InitialRecords            []record
	Targets                   []target
}

type config struct {
	Zones []zone
}

type record struct {
	Name, Type, Value string
	TTL               uint32
}

type result struct {
	Applied bool
	Error   string
}

type release struct {
	Serial  uint32
	Records []record
	File    string
	Created time.Time
	Nodes   map[string]result
}

type controller struct {
	mu       sync.Mutex
	path     string
	zones    map[string]zone
	history  map[string][]release
	client   *http.Client
	password string
	wake     chan struct{}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:9443", "HTTPS listen address")
	configPath := flag.String("config", "controller.json", "zone and node configuration")
	statePath := flag.String("state", "state.json", "persistent release history")
	cert := flag.String("tls-cert", "", "HTTPS certificate")
	key := flag.String("tls-key", "", "HTTPS key")
	caPath := flag.String("node-ca", "", "CA PEM for node API connections")
	clientCert := flag.String("node-client-cert", "", "client certificate for node API connections")
	clientKey := flag.String("node-client-key", "", "client private key for node API connections")
	flag.Parse()
	password := os.Getenv("DNS_ADMIN_PASSWORD")
	if len(password) < 16 || *cert == "" || *key == "" || *caPath == "" {
		log.Fatal("TLS certificate, key, node CA and DNS_ADMIN_PASSWORD (at least 16 bytes) are required")
	}
	var cfg config
	if err := readJSON(*configPath, &cfg); err != nil {
		log.Fatal(err)
	}
	zones, err := loadZones(cfg)
	if err != nil {
		log.Fatal(err)
	}
	caPEM, err := os.ReadFile(*caPath)
	if err != nil {
		log.Fatal(err)
	}
	ca := x509.NewCertPool()
	if !ca.AppendCertsFromPEM(caPEM) {
		log.Fatal("invalid node CA")
	}
	tlsConfig := &tls.Config{RootCAs: ca, MinVersion: tls.VersionTLS12}
	if *clientCert != "" || *clientKey != "" {
		pair, err := tls.LoadX509KeyPair(*clientCert, *clientKey)
		if err != nil {
			log.Fatal(err)
		}
		tlsConfig.Certificates = []tls.Certificate{pair}
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	c := &controller{
		path: *statePath, zones: zones, history: make(map[string][]release),
		client: &http.Client{Transport: transport, Timeout: 40 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		password: password, wake: make(chan struct{}, 1),
	}
	if err := readJSON(c.path, &c.history); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatal(err)
	}
	if c.history == nil {
		log.Fatal("invalid release history")
	}
	go c.run()
	server := &http.Server{Addr: *listen, Handler: http.HandlerFunc(c.serve),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	log.Fatal(server.ListenAndServeTLS(*cert, *key))
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func loadZones(cfg config) (map[string]zone, error) {
	zones := make(map[string]zone)
	for _, z := range cfg.Zones {
		z.Name = dns.CanonicalName(z.Name)
		for _, name := range []string{z.Name, z.Nameserver, z.Contact} {
			if _, ok := dns.IsDomainName(name); !ok || name == "." || strings.ContainsAny(name, "\r\n\t ") {
				return nil, fmt.Errorf("invalid zone configuration name %q", name)
			}
		}
		if _, exists := zones[z.Name]; exists || len(z.Targets) == 0 {
			return nil, fmt.Errorf("zone %s is duplicated or has no targets", z.Name)
		}
		names := make(map[string]bool)
		for i := range z.Targets {
			t := &z.Targets[i]
			u, err := url.Parse(t.URL)
			t.token = os.Getenv(t.TokenEnv)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
				u.Path != "/v1/zone" || u.RawQuery != "" || u.Fragment != "" ||
				t.Name == "" || t.Region == "" || names[t.Name] || len(t.token) < 32 {
				return nil, fmt.Errorf("invalid node %q in zone %s", t.Name, z.Name)
			}
			names[t.Name] = true
		}
		if _, err := zoneFile(z, z.InitialSerial, z.InitialRecords); err != nil {
			return nil, err
		}
		zones[z.Name] = z
	}
	if len(zones) == 0 {
		return nil, errors.New("configure at least one zone")
	}
	return zones, nil
}

func zoneFile(z zone, serial uint32, records []record) (string, error) {
	var out strings.Builder
	fmt.Fprintf(&out, "@ 60 IN SOA %s %s %d 60 30 86400 60\n@ 60 IN NS %s\n",
		dns.Fqdn(z.Nameserver), dns.Fqdn(z.Contact), serial, dns.Fqdn(z.Nameserver))
	types := make(map[string]uint16)
	seen := make(map[string]bool)
	for i, r := range records {
		kind := strings.ToUpper(r.Type)
		if kind != "A" && kind != "AAAA" && kind != "CNAME" && kind != "PTR" {
			return "", fmt.Errorf("record %d has unsupported type", i+1)
		}
		if r.Name == "" || r.Value == "" || r.TTL > 2147483647 ||
			strings.ContainsAny(r.Name+r.Value, " \t\r\n;()\\\"") {
			return "", fmt.Errorf("record %d has invalid fields", i+1)
		}
		owner := z.Name
		if r.Name != "@" {
			owner = dns.CanonicalName(r.Name)
			if !dns.IsFqdn(r.Name) {
				owner = dns.CanonicalName(r.Name + "." + z.Name)
			}
		}
		if !dns.IsSubDomain(z.Name, owner) {
			return "", fmt.Errorf("record %d is outside %s", i+1, z.Name)
		}
		value := r.Value
		if kind == "CNAME" || kind == "PTR" {
			value = dns.Fqdn(value)
		}
		rr, err := dns.NewRR(fmt.Sprintf("%s %d IN %s %s", owner, r.TTL, kind, value))
		if err != nil {
			return "", fmt.Errorf("record %d: %w", i+1, err)
		}
		line := rr.String()
		if seen[line] || types[owner] == dns.TypeCNAME ||
			(kind == "CNAME" && (types[owner] != 0 || owner == z.Name)) {
			return "", fmt.Errorf("record %d conflicts with another record", i+1)
		}
		types[owner] = rr.Header().Rrtype
		seen[line] = true
		fmt.Fprintln(&out, line)
	}
	parser := dns.NewZoneParser(strings.NewReader(out.String()), z.Name, "")
	for _, ok := parser.Next(); ok; _, ok = parser.Next() {
	}
	if err := parser.Err(); err != nil {
		return "", err
	}
	if out.Len() > 1<<20 {
		return "", errors.New("zone exceeds the node upload limit")
	}
	return out.String(), nil
}

func (c *controller) save() error {
	data, err := json.Marshal(c.history)
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	f, err := os.CreateTemp(dir, ".releases-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), c.path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (c *controller) publish(name string, records []record, rollback uint32) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	z, ok := c.zones[name]
	if !ok {
		return 0, errors.New("unknown zone")
	}
	items := c.history[name]
	serial := z.InitialSerial + 1
	if len(items) > 0 {
		serial = items[len(items)-1].Serial + 1
	}
	if serial == 0 {
		serial = 1
	}
	if rollback != 0 {
		found := rollback == z.InitialSerial
		if found {
			records = z.InitialRecords
		}
		for _, old := range items {
			if old.Serial == rollback {
				records, found = old.Records, true
				break
			}
		}
		if !found {
			return 0, errors.New("unknown release")
		}
	}
	file, err := zoneFile(z, serial, records)
	if err != nil {
		return 0, err
	}
	c.history[name] = append(items, release{
		Serial: serial, Records: append([]record{}, records...), File: file, Created: time.Now().UTC(), Nodes: make(map[string]result),
	})
	if err := c.save(); err != nil {
		c.history[name] = items
		return 0, fmt.Errorf("save release: %w", err)
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return serial, nil
}

func (c *controller) run() {
	timer := time.NewTicker(15 * time.Second)
	defer timer.Stop()
	for {
		c.deliver()
		select {
		case <-c.wake:
		case <-timer.C:
		}
	}
}

func (c *controller) deliver() {
	for name, z := range c.zones {
		failedRegion := make(map[string]bool)
		c.mu.Lock()
		items := c.history[name]
		if len(items) == 0 {
			c.mu.Unlock()
			continue
		}
		current := items[len(items)-1]
		c.mu.Unlock()
		for _, t := range z.Targets {
			c.mu.Lock()
			items = c.history[name]
			latest := &items[len(items)-1]
			if latest.Serial != current.Serial {
				c.mu.Unlock()
				break
			}
			applied := latest.Nodes[t.Name].Applied
			c.mu.Unlock()
			if failedRegion[t.Region] || applied {
				continue
			}
			err := c.send(t, current)
			r := result{Applied: err == nil}
			if err != nil {
				r.Error = err.Error()
				failedRegion[t.Region] = true
			}
			c.mu.Lock()
			for i := len(c.history[name]) - 1; i >= 0; i-- {
				entry := &c.history[name][i]
				if entry.Serial == current.Serial {
					entry.Nodes[t.Name] = r
					break
				}
			}
			if err := c.save(); err != nil {
				log.Printf("persist node status: %v", err)
			}
			c.mu.Unlock()
		}
	}
}

func (c *controller) send(t target, release release) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, t.URL, bytes.NewBufferString(release.File))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Content-Type", "text/plain")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var reply struct {
		Serial uint32
		Error  string
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&reply)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("node HTTP %d: %s", resp.StatusCode, reply.Error)
	}
	if err != nil || reply.Serial != release.Serial {
		return fmt.Errorf("node did not confirm serial %d", release.Serial)
	}
	return nil
}

func (c *controller) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	user, pass, ok := r.BasicAuth()
	if !ok || user != "admin" || subtle.ConstantTimeCompare([]byte(pass), []byte(c.password)) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="DNS management"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
		return
	}
	if r.URL.Path != "/api/releases" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		c.mu.Lock()
		defer c.mu.Unlock()
		type nodeInfo struct{ Name, Region string }
		type zoneInfo struct {
			Name           string
			Targets        []nodeInfo
			History        []release
			InitialRecords []record
			InitialSerial  uint32
		}
		all := make([]zoneInfo, 0, len(c.zones))
		for name, z := range c.zones {
			info := zoneInfo{Name: name, InitialRecords: z.InitialRecords, InitialSerial: z.InitialSerial}
			for _, t := range z.Targets {
				info.Targets = append(info.Targets, nodeInfo{t.Name, t.Region})
			}
			for _, item := range c.history[name] {
				item.File = ""
				info.History = append(info.History, item)
			}
			all = append(all, info)
		}
		_ = json.NewEncoder(w).Encode(all)
	case http.MethodPost:
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
			return
		}
		var input struct {
			Zone     string
			Records  []record
			Rollback uint32
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := decoder.Decode(&input); err != nil || decoder.Decode(new(any)) != io.EOF {
			http.Error(w, "invalid release request", http.StatusBadRequest)
			return
		}
		serial, err := c.publish(input.Zone, input.Records, input.Rollback)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]uint32{"serial": serial})
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
