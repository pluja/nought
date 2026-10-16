// SPDX-License-Identifier: AGPL-3.0-or-later

// Package autoconfig finds IMAP and SMTP settings from an email address,
// like Thunderbird, but privately: a built-in copy of Mozilla's ISPDB first
// (offline), and only if allowed, DNS, the provider's own autoconfig file,
// and a verified guess. Mozilla's online service is never contacted.
package autoconfig

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	_ "embed" // the built-in ISPDB snapshot
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/pluja/nought/internal/config"
)

//go:embed ispdb.json.gz
var ispdb []byte

// onlineTimeout bounds all online steps together.
const onlineTimeout = 45 * time.Second

// Sources, in the order they are tried.
const (
	SourceBuiltIn  = "built-in provider list"
	SourceSRV      = "DNS SRV records"
	SourceMX       = "built-in provider list, via the domain's MX records"
	SourceProvider = "the provider's autoconfig file"
	SourceGuess    = "guessed and verified by a TLS connection"
)

// Errors.
var (
	ErrNotFound  = errors.New("no settings found")
	ErrOAuthOnly = errors.New("this provider requires OAuth sign-in, which nought does not support yet")
)

// Result is what discovery found.
type Result struct {
	Provider  string        `json:"provider,omitempty"`
	Source    string        `json:"source"`
	IMAP      config.Server `json:"imap"`
	SMTP      config.Server `json:"smtp"`
	Username  string        `json:"username"`
	Contacted []string      `json:"contacted"` // network destinations used, for transparency
}

// Network is how discovery reaches the outside world; tests replace it.
type Network struct {
	LookupSRV func(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
	LookupMX  func(ctx context.Context, name string) ([]*net.MX, error)
	HTTP      *http.Client
	// ProbeTLS reports whether host:port accepts a TLS connection with a
	// certificate valid for host.
	ProbeTLS func(ctx context.Context, host string, port int) bool
}

// DefaultNetwork uses the system resolver and the internet, carefully:
// HTTPS only, no redirects to other schemes, short timeouts.
func DefaultNetwork() Network {
	var r net.Resolver
	return Network{
		LookupSRV: r.LookupSRV,
		LookupMX:  r.LookupMX,
		HTTP: &http.Client{
			Timeout: 10 * time.Second,
			// Redirects must stay on HTTPS and within the original domain.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				orig := via[0].URL.Hostname()
				host := req.URL.Hostname()
				base := strings.TrimPrefix(orig, "autoconfig.")
				if req.URL.Scheme != "https" || len(via) >= 3 || host != base && !strings.HasSuffix(host, "."+base) {
					return errors.New("refusing redirect")
				}
				return nil
			},
		},
		ProbeTLS: func(ctx context.Context, host string, port int) bool {
			d := tls.Dialer{Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
			if err != nil {
				return false
			}
			_ = c.Close()
			return true
		},
	}
}

// Discover finds settings for email. With online false, only the built-in
// list is used and nothing leaves the machine.
func Discover(ctx context.Context, email string, online bool, network Network) (Result, error) {
	addr, err := mail.ParseAddress(email)
	if err != nil || strings.ContainsAny(email, "<>") {
		return Result{}, fmt.Errorf("%q is not a plain email address", email)
	}
	local, domain, _ := strings.Cut(strings.ToLower(addr.Address), "@")
	if !validHost(domain) {
		return Result{}, fmt.Errorf("%q has no usable domain", email)
	}
	db, err := loadDB()
	if err != nil {
		return Result{}, err
	}
	if p, ok := byDomain(db, domain); ok {
		return fromProvider(p, SourceBuiltIn, local, domain, nil)
	}
	if !online {
		return Result{}, ErrNotFound
	}

	ctx, cancel := context.WithTimeout(ctx, onlineTimeout)
	defer cancel()
	var contacted []string
	if res, ok := fromSRV(ctx, network, domain, local, &contacted); ok {
		return res, nil
	}
	if p, ok := fromMX(ctx, network, db, domain, &contacted); ok {
		res, err := fromProvider(p, SourceMX, local, domain, contacted)
		if err == nil || errors.Is(err, ErrOAuthOnly) {
			return res, err
		}
	}
	if p, ok := fromProviderFile(ctx, network, domain, &contacted); ok {
		res, err := fromProvider(p, SourceProvider, local, domain, contacted)
		if !errors.Is(err, ErrNotFound) {
			return res, err
		}
	}
	if res, ok := guess(ctx, network, domain, local, &contacted); ok {
		return res, nil
	}
	return Result{Contacted: contacted}, ErrNotFound
}

func loadDB() ([]Provider, error) {
	zr, err := gzip.NewReader(bytes.NewReader(ispdb))
	if err != nil {
		return nil, fmt.Errorf("built-in provider list: %w", err)
	}
	var db []Provider
	if err := json.NewDecoder(zr).Decode(&db); err != nil {
		return nil, fmt.Errorf("built-in provider list: %w", err)
	}
	return db, nil
}

func byDomain(db []Provider, domain string) (Provider, bool) {
	for _, p := range db {
		if slices.Contains(p.Domains, domain) {
			return p, true
		}
	}
	return Provider{}, false
}

// fromProvider picks the first usable IMAP (and SMTP) server: TLS or
// STARTTLS only, with password authentication.
func fromProvider(p Provider, source, local, domain string, contacted []string) (Result, error) {
	res := Result{Provider: p.Name, Source: source, Contacted: contacted}
	oauthOnly := false
	for _, e := range p.IMAP {
		srv, ok := secureServer(e)
		if !ok {
			continue
		}
		if !passwordAuth(e.Auth) {
			oauthOnly = true
			continue
		}
		res.IMAP, res.Username = srv, expand(e.Username, local, domain)
		break
	}
	if res.IMAP == (config.Server{}) {
		if oauthOnly {
			return res, ErrOAuthOnly
		}
		return res, ErrNotFound
	}
	for _, e := range p.SMTP {
		if srv, ok := secureServer(e); ok && passwordAuth(e.Auth) {
			res.SMTP = srv
			break
		}
	}
	if res.Username == "" {
		res.Username = local + "@" + domain
	}
	return res, nil
}

func secureServer(e Entry) (config.Server, bool) {
	switch e.Socket {
	case "SSL", "TLS":
		return config.Server{Host: e.Host, Port: e.Port, TLS: config.TLSImplicit}, true
	case "STARTTLS":
		return config.Server{Host: e.Host, Port: e.Port, TLS: config.TLSStartTLS}, true
	default:
		return config.Server{}, false // plaintext is never offered
	}
}

func passwordAuth(auth []string) bool {
	if len(auth) == 0 {
		return true
	}
	for _, a := range auth {
		switch strings.ToLower(a) {
		case "password-cleartext", "password-encrypted", "plain", "secure":
			return true
		}
	}
	return false
}

func expand(template, local, domain string) string {
	r := strings.NewReplacer("%EMAILADDRESS%", local+"@"+domain, "%EMAILLOCALPART%", local, "%EMAILDOMAIN%", domain)
	if out := r.Replace(template); validUsernameTemplate(out) {
		return out
	}
	return local + "@" + domain
}

// Trusted reports whether the settings come from nought's built-in list or
// point at hosts within the email's own domain on standard ports. Anything
// else (unauthenticated DNS answers, redirected files) deserves a warning
// before a password is sent there.
func (r Result) Trusted(domain string) bool {
	if r.Source == SourceBuiltIn || r.Source == SourceMX {
		return true
	}
	inDomain := func(host string) bool { return host == domain || strings.HasSuffix(host, "."+domain) }
	standard := map[int]bool{993: true, 143: true, 465: true, 587: true}
	if !inDomain(r.IMAP.Host) || !standard[r.IMAP.Port] {
		return false
	}
	return r.SMTP.Host == "" || inDomain(r.SMTP.Host) && standard[r.SMTP.Port]
}

// fromSRV implements RFC 6186 and RFC 8314.
func fromSRV(ctx context.Context, n Network, domain, local string, contacted *[]string) (Result, bool) {
	*contacted = append(*contacted, "DNS: SRV records of "+domain)
	lookup := func(service string) (string, int, bool) {
		_, addrs, err := n.LookupSRV(ctx, service, "tcp", domain)
		if err != nil {
			return "", 0, false
		}
		for _, a := range addrs {
			host := strings.TrimSuffix(strings.ToLower(a.Target), ".")
			if host != "" && validHost(host) && a.Port != 0 { // "." means service not offered
				return host, int(a.Port), true
			}
		}
		return "", 0, false
	}
	res := Result{Source: SourceSRV, Username: local + "@" + domain}
	if host, port, ok := lookup("imaps"); ok {
		res.IMAP = config.Server{Host: host, Port: port, TLS: config.TLSImplicit}
	} else if host, port, ok := lookup("imap"); ok {
		res.IMAP = config.Server{Host: host, Port: port, TLS: config.TLSStartTLS}
	} else {
		return Result{}, false
	}
	if host, port, ok := lookup("submissions"); ok {
		res.SMTP = config.Server{Host: host, Port: port, TLS: config.TLSImplicit}
	} else if host, port, ok := lookup("submission"); ok {
		res.SMTP = config.Server{Host: host, Port: port, TLS: config.TLSStartTLS}
	}
	res.Contacted = *contacted
	return res, true
}

// fromMX finds hosted mail (e.g. a custom domain at a big provider) by
// looking up the MX host's domain in the built-in list.
func fromMX(ctx context.Context, n Network, db []Provider, domain string, contacted *[]string) (Provider, bool) {
	*contacted = append(*contacted, "DNS: MX records of "+domain)
	mxs, err := n.LookupMX(ctx, domain)
	if err != nil {
		return Provider{}, false
	}
	for _, mx := range mxs {
		host := strings.TrimSuffix(strings.ToLower(mx.Host), ".")
		// Try the host's parent domains: mx1.mail.example.net → mail.example.net → example.net.
		for d := host; strings.Count(d, ".") >= 1; d = d[strings.Index(d, ".")+1:] {
			if p, ok := byDomain(db, d); ok {
				return p, true
			}
		}
	}
	return Provider{}, false
}

func fromProviderFile(ctx context.Context, n Network, domain string, contacted *[]string) (Provider, bool) {
	for _, url := range []string{
		"https://autoconfig." + domain + "/mail/config-v1.1.xml",
		"https://" + domain + "/.well-known/autoconfig/mail/config-v1.1.xml",
	} {
		*contacted = append(*contacted, "HTTPS: "+url)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		resp, err := n.HTTP.Do(req)
		if err != nil {
			continue
		}
		p, err := ParseXML(resp.Body)
		_ = resp.Body.Close()
		if err == nil && resp.StatusCode == http.StatusOK && len(p.IMAP) > 0 {
			return p, true
		}
	}
	return Provider{}, false
}

// guess tries common host names; a guess counts only if a TLS connection
// with a valid certificate succeeds.
func guess(ctx context.Context, n Network, domain, local string, contacted *[]string) (Result, bool) {
	res := Result{Source: SourceGuess, Username: local + "@" + domain}
	for _, host := range []string{"imap." + domain, "mail." + domain, domain} {
		*contacted = append(*contacted, "TLS: "+host+":993")
		if n.ProbeTLS(ctx, host, 993) {
			res.IMAP = config.Server{Host: host, Port: 993, TLS: config.TLSImplicit}
			break
		}
	}
	if res.IMAP == (config.Server{}) {
		return Result{}, false
	}
	for _, host := range []string{"smtp." + domain, "mail." + domain, domain} {
		*contacted = append(*contacted, "TLS: "+host+":465")
		if n.ProbeTLS(ctx, host, 465) {
			res.SMTP = config.Server{Host: host, Port: 465, TLS: config.TLSImplicit}
			break
		}
	}
	res.Contacted = *contacted
	return res, true
}
