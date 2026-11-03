// Package cftunnel gives the access-code-gated listener a fixed address on
// the person's own domain (https://decurix.example.com), through a named
// Cloudflare Tunnel, in place of a trycloudflare.com quick tunnel whose URL
// changes on every restart.
//
// Configure does the Cloudflare side once, through the API: find the zone,
// create (or reuse) a remotely-managed tunnel, route the hostname to this
// machine's gated port, point a proxied CNAME at it. Run then keeps a
// cloudflared connector for that tunnel running as a child process. Several
// connectors may run the same tunnel - Cloudflare sends each visitor to one
// that is up - which is what later lets every node of a cluster serve the
// same link.
//
// Only the gated listener is ever routed. The API token is sealed in the
// database (internal/sealed) and never served back; the connector token is
// only handed to the connector's own environment.
package cftunnel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sealed"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// KeyFile is the name of the key, in the data directory, that seals the
// Cloudflare tokens in the database.
const KeyFile = "cloudflare.key"

// DefaultSubdomain is the name the link gets under the domain when none is given.
const DefaultSubdomain = "decurix"

// statusTTL is how long a read of the tunnel's live connections is reused.
const statusTTL = 20 * time.Second

var (
	domainRe    = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	subdomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// ErrInvalid is a domain or subdomain that is not a hostname, or a missing token.
var ErrInvalid = errors.New("cftunnel: invalid settings")

// ErrUnsealed is Configure's refusal when the key file that seals the saved tokens could not be loaded or written:
// they are never stored in plain text (RW1-F-06, as for the model keys, C-19).
var ErrUnsealed = errors.New("cftunnel: no key file to seal the tokens with")

var schema = []string{
	`CREATE TABLE cftunnel_config (
		id              INTEGER PRIMARY KEY CHECK (id = 1),
		api_token       TEXT NOT NULL,
		domain          TEXT NOT NULL,
		hostname        TEXT NOT NULL,
		account_id      TEXT NOT NULL,
		zone_id         TEXT NOT NULL,
		tunnel_id       TEXT NOT NULL,
		tunnel_name     TEXT NOT NULL,
		connector_token TEXT NOT NULL,
		updated_ns      INTEGER NOT NULL
	)`,
}

// config is the saved setup, tokens unsealed.
type config struct {
	apiToken       string
	domain         string
	hostname       string
	accountID      string
	zoneID         string
	tunnelID       string
	tunnelName     string
	connectorToken string
	updated        time.Time
}

// Status is what the dashboard shows about the named tunnel.
type Status struct {
	Configured bool      `json:"configured"`
	URL        string    `json:"url,omitempty"`
	Hostname   string    `json:"hostname,omitempty"`
	Domain     string    `json:"domain,omitempty"`
	TunnelName string    `json:"tunnel_name,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitzero"`
	// Connector is this machine's cloudflared: running, starting, stopped,
	// missing (not installed) or failed.
	Connector      string `json:"connector,omitempty"`
	ConnectorError string `json:"connector_error,omitempty"`
	// Connections is how many connections Cloudflare holds open to the
	// tunnel, from every connector; -1 when it could not be asked.
	Connections int    `json:"connections"`
	Cloud       string `json:"cloud_status,omitempty"`
}

// Manager is the named tunnel's settings and this machine's connector.
type Manager struct {
	db        *sql.DB
	key       []byte
	localPort int

	// APIBase overrides Cloudflare's API address (tests).
	APIBase string

	// mu guards cfg and the status cache.
	mu        sync.Mutex
	cfg       *config
	cloud     remoteTunnel
	cloudErr  error
	cloudTime time.Time

	conn *connector
}

// NewManager opens the saved setup on db. key seals the tokens (nil stores
// them as they are - tests); localPort is the gated listener's port, what
// the tunnel routes to.
func NewManager(db *sql.DB, key []byte, localPort int) (*Manager, error) {
	if err := sqlitedb.Migrate(db, "cftunnel", schema); err != nil {
		return nil, err
	}

	m := &Manager{db: db, key: key, localPort: localPort, conn: newConnector()}

	cfg, err := m.load()
	if errors.Is(err, sealed.ErrUnreadable) {
		// A restored backup carries no key files (internal/httpapi/backup.go):
		// on another install the saved tokens no longer open. That is "set the
		// tunnel up again", not a control plane that will not start.
		log.Printf("warning: %v", err)
		cfg, err = nil, nil
	}
	if err != nil {
		return nil, err
	}

	m.cfg = cfg

	return m, nil
}

// load reads the saved setup, or nil when there is none.
func (m *Manager) load() (*config, error) {
	var c config
	var apiToken, connectorToken string
	var updated int64

	err := m.db.QueryRow(`SELECT api_token, domain, hostname, account_id, zone_id, tunnel_id, tunnel_name, connector_token, updated_ns
		FROM cftunnel_config WHERE id = 1`).Scan(&apiToken, &c.domain, &c.hostname, &c.accountID, &c.zoneID,
		&c.tunnelID, &c.tunnelName, &connectorToken, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if c.apiToken, err = sealed.Open(m.key, apiToken); err != nil {
		return nil, fmt.Errorf("cftunnel: the saved API token cannot be read (%w); set the tunnel up again", err)
	}
	if c.connectorToken, err = sealed.Open(m.key, connectorToken); err != nil {
		return nil, fmt.Errorf("cftunnel: the saved connector token cannot be read (%w); set the tunnel up again", err)
	}

	c.updated = sqlitedb.FromNS(updated)

	return &c, nil
}

// save writes c as the one saved setup.
func (m *Manager) save(c *config) error {
	apiToken, err := sealed.Seal(m.key, c.apiToken)
	if err != nil {
		return err
	}

	connectorToken, err := sealed.Seal(m.key, c.connectorToken)
	if err != nil {
		return err
	}

	_, err = m.db.Exec(`INSERT INTO cftunnel_config
		(id, api_token, domain, hostname, account_id, zone_id, tunnel_id, tunnel_name, connector_token, updated_ns)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			api_token = excluded.api_token, domain = excluded.domain, hostname = excluded.hostname,
			account_id = excluded.account_id, zone_id = excluded.zone_id, tunnel_id = excluded.tunnel_id,
			tunnel_name = excluded.tunnel_name, connector_token = excluded.connector_token,
			updated_ns = excluded.updated_ns`,
		apiToken, c.domain, c.hostname, c.accountID, c.zoneID, c.tunnelID, c.tunnelName, connectorToken,
		sqlitedb.ToNS(c.updated))

	return err
}

// normalizeDomain turns what a person typed ("https://Example.com/") into a
// bare lowercase domain.
func normalizeDomain(domain string) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimPrefix(domain, "http://")

	return strings.Trim(domain, "/.")
}

// tunnelNameFor is the tunnel's name on the account, derived from the
// hostname so two Decurix installs on one account do not share a tunnel by
// accident.
func tunnelNameFor(hostname string) string {
	return "decurix-" + strings.ReplaceAll(hostname, ".", "-")
}

// Configure sets the named tunnel up on Cloudflare for subdomain.domain and
// saves it, replacing any earlier setup, then (re)starts this machine's
// connector. It is safe to run again: an existing tunnel of the same name
// is reused and a DNS record it made earlier is repointed, never another's.
//
// The token needs Account > Cloudflare Tunnel > Edit, Zone > DNS > Edit and
// Zone > Zone > Read on the domain.
func (m *Manager) Configure(ctx context.Context, apiToken, domain, subdomain string) (Status, error) {
	apiToken = strings.TrimSpace(apiToken)
	domain = normalizeDomain(domain)
	subdomain = strings.ToLower(strings.TrimSpace(subdomain))

	if subdomain == "" {
		subdomain = DefaultSubdomain
	}

	if apiToken == "" {
		m.mu.Lock()
		if m.cfg != nil {
			apiToken = m.cfg.apiToken // re-running setup without retyping the token
		}
		m.mu.Unlock()
	}

	if m.db != nil && m.key == nil {
		return Status{}, ErrUnsealed
	}
	if apiToken == "" || !domainRe.MatchString(domain) || !subdomainRe.MatchString(subdomain) {
		return Status{}, ErrInvalid
	}

	hostname := subdomain + "." + domain
	api := newClient(m.APIBase, apiToken)

	z, err := api.zoneByName(ctx, domain)
	if err != nil {
		return Status{}, err
	}

	name := tunnelNameFor(hostname)

	t, found, err := api.findTunnel(ctx, z.Account.ID, name)
	if err != nil {
		return Status{}, err
	}

	if !found {
		if t, err = api.createTunnel(ctx, z.Account.ID, name); err != nil {
			return Status{}, err
		}
	}

	service := fmt.Sprintf("http://127.0.0.1:%d", m.localPort)
	if err := api.putIngress(ctx, z.Account.ID, t.ID, hostname, service); err != nil {
		return Status{}, err
	}

	if err := api.pointCNAME(ctx, z.ID, hostname, t.ID+".cfargotunnel.com"); err != nil {
		return Status{}, err
	}

	token, err := api.tunnelToken(ctx, z.Account.ID, t.ID)
	if err != nil {
		return Status{}, err
	}

	cfg := &config{
		apiToken:       apiToken,
		domain:         domain,
		hostname:       hostname,
		accountID:      z.Account.ID,
		zoneID:         z.ID,
		tunnelID:       t.ID,
		tunnelName:     name,
		connectorToken: token,
		updated:        time.Now(),
	}

	if err := m.save(cfg); err != nil {
		return Status{}, err
	}

	m.mu.Lock()
	m.cfg = cfg
	m.cloudTime = time.Time{}
	m.mu.Unlock()

	m.conn.run(token)

	return m.Status(ctx), nil
}

// Disable stops this machine's connector and forgets the setup. The tunnel
// and its DNS record stay on Cloudflare (setting it up again reuses them);
// delete them there to take the link down for good.
func (m *Manager) Disable() error {
	m.conn.run("")

	if _, err := m.db.Exec(`DELETE FROM cftunnel_config WHERE id = 1`); err != nil {
		return err
	}

	m.mu.Lock()
	m.cfg = nil
	m.mu.Unlock()

	return nil
}

// URL returns the link the tunnel serves, or "" when none is set up.
func (m *Manager) URL() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cfg == nil {
		return ""
	}

	return "https://" + m.cfg.hostname
}

// Status reports the setup, this machine's connector, and how many
// connections Cloudflare has to the tunnel.
func (m *Manager) Status(ctx context.Context) Status {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()

	if cfg == nil {
		return Status{Configured: false, Connections: -1}
	}

	st := Status{
		Configured:  true,
		URL:         "https://" + cfg.hostname,
		Hostname:    cfg.hostname,
		Domain:      cfg.domain,
		TunnelName:  cfg.tunnelName,
		UpdatedAt:   cfg.updated,
		Connections: -1,
	}

	st.Connector, st.ConnectorError = m.conn.state()

	if t, err := m.cloudTunnel(ctx, cfg); err == nil {
		st.Connections = len(t.Connections)
		st.Cloud = t.Status
	}

	return st
}

// cloudTunnel reads the tunnel's live state from Cloudflare, cached for statusTTL.
func (m *Manager) cloudTunnel(ctx context.Context, cfg *config) (remoteTunnel, error) {
	m.mu.Lock()
	if time.Since(m.cloudTime) < statusTTL {
		t, err := m.cloud, m.cloudErr
		m.mu.Unlock()
		return t, err
	}
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	t, err := newClient(m.APIBase, cfg.apiToken).getTunnel(ctx, cfg.accountID, cfg.tunnelID)

	m.mu.Lock()
	m.cloud, m.cloudErr, m.cloudTime = t, err, time.Now()
	m.mu.Unlock()

	return t, err
}

// Run keeps this machine's connector running for the saved setup until ctx
// ends. Meant for its own goroutine (cmd/decurix/main.go).
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()

	if cfg != nil {
		m.conn.run(cfg.connectorToken)
	}

	<-ctx.Done()
	m.conn.run("")
}
