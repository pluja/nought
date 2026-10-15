// SPDX-License-Identifier: AGPL-3.0-or-later

package autoconfig

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Provider is one mail provider's settings, in the shape of Mozilla's
// autoconfig ("clientConfig") format.
type Provider struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
	IMAP    []Entry  `json:"imap,omitempty"`
	SMTP    []Entry  `json:"smtp,omitempty"`
}

// Entry is one server a provider offers.
type Entry struct {
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Socket   string   `json:"socket"`   // "SSL", "STARTTLS" or "plain"
	Username string   `json:"username"` // template: %EMAILADDRESS%, %EMAILLOCALPART%, %EMAILDOMAIN%
	Auth     []string `json:"auth,omitempty"`
}

type clientConfig struct {
	Providers []struct {
		ID       string   `xml:"id,attr"`
		Domains  []string `xml:"domain"`
		Name     string   `xml:"displayName"`
		Incoming []server `xml:"incomingServer"`
		Outgoing []server `xml:"outgoingServer"`
	} `xml:"emailProvider"`
}

type server struct {
	Type     string   `xml:"type,attr"`
	Host     string   `xml:"hostname"`
	Port     int      `xml:"port"`
	Socket   string   `xml:"socketType"`
	Username string   `xml:"username"`
	Auth     []string `xml:"authentication"`
}

// maxXML bounds an autoconfig document.
const maxXML = 256 << 10

// ParseXML reads an autoconfig document. Only IMAP and SMTP servers are
// kept; everything is validated, and anything odd is dropped.
func ParseXML(r io.Reader) (Provider, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxXML+1))
	if err != nil {
		return Provider{}, err
	}
	if len(data) > maxXML {
		return Provider{}, errors.New("autoconfig document too large")
	}
	var doc clientConfig
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = true
	if err := dec.Decode(&doc); err != nil {
		return Provider{}, fmt.Errorf("parse autoconfig: %w", err)
	}
	if len(doc.Providers) == 0 {
		return Provider{}, errors.New("autoconfig document has no emailProvider")
	}
	src := doc.Providers[0]
	p := Provider{ID: strings.TrimSpace(src.ID), Name: strings.TrimSpace(src.Name)}
	for _, d := range src.Domains {
		if d = strings.ToLower(strings.TrimSpace(d)); validHost(d) {
			p.Domains = append(p.Domains, d)
		}
	}
	for _, s := range src.Incoming {
		if strings.EqualFold(s.Type, "imap") {
			if e, ok := entry(s); ok {
				p.IMAP = append(p.IMAP, e)
			}
		}
	}
	for _, s := range src.Outgoing {
		if strings.EqualFold(s.Type, "smtp") {
			if e, ok := entry(s); ok {
				p.SMTP = append(p.SMTP, e)
			}
		}
	}
	return p, nil
}

func entry(s server) (Entry, bool) {
	e := Entry{
		Host:     strings.ToLower(strings.TrimSpace(s.Host)),
		Port:     s.Port,
		Socket:   strings.ToUpper(strings.TrimSpace(s.Socket)),
		Username: strings.TrimSpace(s.Username),
	}
	for _, a := range s.Auth {
		e.Auth = append(e.Auth, strings.TrimSpace(a))
	}
	if !validHost(e.Host) || e.Port < 1 || e.Port > 65535 {
		return Entry{}, false
	}
	return e, true
}

// validHost accepts plain DNS names (no placeholders, no ports, no URLs).
func validHost(h string) bool {
	if h == "" || len(h) > 253 || strings.Trim(h, ".") != h {
		return false
	}
	for _, r := range h {
		if !isHostRune(r) {
			return false
		}
	}
	return strings.Contains(h, ".")
}

func isHostRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.'
}
