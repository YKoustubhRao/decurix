package cftunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultAPIBase is Cloudflare's v4 API.
const DefaultAPIBase = "https://api.cloudflare.com/client/v4"

// maxAPIResponseBytes bounds one API response; none of the calls here
// return more than a few kilobytes.
const maxAPIResponseBytes = 4 << 20

// APIError is Cloudflare refusing a call, with its own message, which is
// usually what a person needs to fix it ("Authentication error", "Record
// already exists").
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cloudflare: %s (HTTP %d)", e.Message, e.Status)
}

// client is the handful of Cloudflare API calls a named tunnel needs.
type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(base, token string) *client {
	if base == "" {
		base = DefaultAPIBase
	}

	return &client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

// envelope is every Cloudflare v4 response.
type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiMessage    `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// call sends one request and decodes its result into out (which may be nil).
func (c *client) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes))
	if err != nil {
		return fmt.Errorf("cloudflare: reading the response: %w", err)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return &APIError{Status: resp.StatusCode, Message: "unexpected response"}
	}

	if !env.Success || resp.StatusCode >= 300 {
		messages := make([]string, 0, len(env.Errors))
		for _, m := range env.Errors {
			messages = append(messages, m.Message)
		}

		message := strings.Join(messages, "; ")
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}

		return &APIError{Status: resp.StatusCode, Message: message}
	}

	if out == nil {
		return nil
	}

	return json.Unmarshal(env.Result, out)
}

// zone is the part of a Cloudflare zone this package reads.
type zone struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Account struct {
		ID string `json:"id"`
	} `json:"account"`
}

// ErrZoneNotFound is a domain the token cannot see: not on this Cloudflare
// account, or the token lacks Zone Read on it.
var ErrZoneNotFound = errors.New("cloudflare: the domain is not on this account, or the token cannot read it")

// zoneByName finds the zone for domain.
func (c *client) zoneByName(ctx context.Context, domain string) (zone, error) {
	var zones []zone
	if err := c.call(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(domain), nil, &zones); err != nil {
		return zone{}, err
	}

	if len(zones) == 0 {
		return zone{}, ErrZoneNotFound
	}

	return zones[0], nil
}

// remoteTunnel is a Cloudflare Tunnel as the API lists it.
type remoteTunnel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Connections []struct {
		ID         string `json:"id"`
		OriginIP   string `json:"origin_ip"`
		OpenedAt   string `json:"opened_at"`
		ColoName   string `json:"colo_name"`
		ClientID   string `json:"client_id"`
		IsPending  bool   `json:"is_pending_reconnect"`
		ClientVers string `json:"client_version"`
	} `json:"connections"`
}

// findTunnel returns the live tunnel called name, if there is one.
func (c *client) findTunnel(ctx context.Context, account, name string) (remoteTunnel, bool, error) {
	var tunnels []remoteTunnel

	path := "/accounts/" + url.PathEscape(account) + "/cfd_tunnel?is_deleted=false&name=" + url.QueryEscape(name)
	if err := c.call(ctx, http.MethodGet, path, nil, &tunnels); err != nil {
		return remoteTunnel{}, false, err
	}

	for _, t := range tunnels {
		if t.Name == name {
			return t, true, nil
		}
	}

	return remoteTunnel{}, false, nil
}

// createTunnel creates a remotely-managed tunnel (its routes are set with
// putIngress, not a local config file).
func (c *client) createTunnel(ctx context.Context, account, name string) (remoteTunnel, error) {
	var t remoteTunnel

	body := map[string]string{"name": name, "config_src": "cloudflare"}
	err := c.call(ctx, http.MethodPost, "/accounts/"+url.PathEscape(account)+"/cfd_tunnel", body, &t)

	return t, err
}

// getTunnel reads one tunnel, with its live connections.
func (c *client) getTunnel(ctx context.Context, account, id string) (remoteTunnel, error) {
	var t remoteTunnel

	err := c.call(ctx, http.MethodGet, "/accounts/"+url.PathEscape(account)+"/cfd_tunnel/"+url.PathEscape(id), nil, &t)

	return t, err
}

// tunnelToken returns the token a connector runs the tunnel with.
func (c *client) tunnelToken(ctx context.Context, account, id string) (string, error) {
	var token string

	err := c.call(ctx, http.MethodGet, "/accounts/"+url.PathEscape(account)+"/cfd_tunnel/"+url.PathEscape(id)+"/token", nil, &token)

	return token, err
}

// putIngress routes hostname to service through the tunnel; anything else
// reaching it gets a 404.
func (c *client) putIngress(ctx context.Context, account, id, hostname, service string) error {
	body := map[string]any{
		"config": map[string]any{
			"ingress": []map[string]string{
				{"hostname": hostname, "service": service},
				{"service": "http_status:404"},
			},
		},
	}

	return c.call(ctx, http.MethodPut, "/accounts/"+url.PathEscape(account)+"/cfd_tunnel/"+url.PathEscape(id)+"/configurations", body, nil)
}

// dnsRecord is the part of a DNS record this package reads.
type dnsRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment"`
}

// recordComment marks a DNS record as this package's, so a later setup
// (a new tunnel, after the old one was deleted) may repoint it.
const recordComment = "Decurix tunnel"

// ErrNameTaken is a hostname that already has a DNS record this package did
// not make (an A record, or a CNAME to something else) - never overwritten.
var ErrNameTaken = errors.New("cloudflare: that hostname already has a DNS record pointing somewhere else; pick another name or delete the record")

// pointCNAME makes hostname a proxied CNAME to target: creating the record,
// leaving it when it already points there, or repointing one this package
// made earlier. Any other record for the name is someone else's, and is
// refused rather than overwritten.
func (c *client) pointCNAME(ctx context.Context, zoneID, hostname, target string) error {
	var records []dnsRecord

	path := "/zones/" + url.PathEscape(zoneID) + "/dns_records?name=" + url.QueryEscape(hostname)
	if err := c.call(ctx, http.MethodGet, path, nil, &records); err != nil {
		return err
	}

	for _, r := range records {
		if r.Type != "CNAME" {
			continue
		}

		if strings.EqualFold(r.Content, target) && r.Proxied {
			return nil
		}

		// Ours (pointing at an older tunnel, or left unproxied): repoint it.
		if strings.EqualFold(r.Content, target) || r.Comment == recordComment {
			body := map[string]any{"content": target, "proxied": true, "comment": recordComment}
			return c.call(ctx, http.MethodPatch, "/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(r.ID), body, nil)
		}
	}

	if len(records) > 0 {
		return ErrNameTaken
	}

	body := map[string]any{
		"type":    "CNAME",
		"name":    hostname,
		"content": target,
		"proxied": true,
		"comment": recordComment,
	}

	return c.call(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/dns_records", body, nil)
}
