// SPDX-License-Identifier: AGPL-3.0-or-later

package autoconfig

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"nought/internal/config"
)

// offline is a Network that fails the test if anything is contacted.
func offline(t *testing.T) Network {
	return Network{
		LookupSRV: func(context.Context, string, string, string) (string, []*net.SRV, error) {
			t.Fatal("DNS used offline")
			return "", nil, nil
		},
		LookupMX: func(context.Context, string) ([]*net.MX, error) {
			t.Fatal("DNS used offline")
			return nil, nil
		},
		ProbeTLS: func(context.Context, string, int) bool { t.Fatal("network used offline"); return false },
	}
}

// fake is a Network with canned answers.
type fake struct {
	srv   map[string][]*net.SRV // "service.domain"
	mx    map[string][]*net.MX
	probe map[string]bool // "host:port"
	http  *http.Client
}

func (f fake) network() Network {
	return Network{
		LookupSRV: func(_ context.Context, service, _, name string) (string, []*net.SRV, error) {
			if r, ok := f.srv[service+"."+name]; ok {
				return "", r, nil
			}
			return "", nil, errors.New("no such record")
		},
		LookupMX: func(_ context.Context, name string) ([]*net.MX, error) {
			if r, ok := f.mx[name]; ok {
				return r, nil
			}
			return nil, errors.New("no such record")
		},
		HTTP: f.httpClient(),
		ProbeTLS: func(_ context.Context, host string, port int) bool {
			return f.probe[net.JoinHostPort(host, fmt.Sprint(port))]
		},
	}
}

func (f fake) httpClient() *http.Client {
	if f.http != nil {
		return f.http
	}
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBuiltInListIsOffline(t *testing.T) {
	res, err := Discover(t.Context(), "Someone@Posteo.de", false, offline(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != SourceBuiltIn || res.IMAP != (config.Server{Host: "posteo.de", Port: 993, TLS: "implicit"}) ||
		res.Username != "someone@posteo.de" || res.SMTP.Host == "" || len(res.Contacted) != 0 {
		t.Errorf("posteo = %+v", res)
	}
	if _, err := Discover(t.Context(), "me@unknown.example", false, offline(t)); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown offline = %v", err)
	}
}

func TestBuiltInSnapshotLoads(t *testing.T) {
	db, err := loadDB()
	if err != nil || len(db) < 50 {
		t.Fatalf("snapshot: %d providers, %v", len(db), err)
	}
	for _, domain := range []string{"gmx.net", "yahoo.com", "gmail.com"} {
		if _, ok := byDomain(db, domain); !ok {
			t.Errorf("%s missing from the snapshot", domain)
		}
	}
	for _, p := range db {
		for _, e := range append(slices.Clone(p.IMAP), p.SMTP...) {
			if !validHost(e.Host) {
				t.Errorf("%s: bad host %q", p.ID, e.Host)
			}
		}
	}
}

func TestSRV(t *testing.T) {
	f := fake{srv: map[string][]*net.SRV{
		"imaps.example.org":       {{Target: "mail.example.org.", Port: 993}},
		"submissions.example.org": {{Target: "smtp.example.org.", Port: 465}},
		"imap.starttls.example":   {{Target: "imap.starttls.example.", Port: 143}},
		"imaps.disabled.example":  {{Target: ".", Port: 0}},
	}}
	res, err := Discover(t.Context(), "me@example.org", true, f.network())
	if err != nil || res.Source != SourceSRV || res.IMAP != (config.Server{Host: "mail.example.org", Port: 993, TLS: "implicit"}) ||
		res.SMTP != (config.Server{Host: "smtp.example.org", Port: 465, TLS: "implicit"}) {
		t.Fatalf("srv = %+v, %v", res, err)
	}
	if !slices.Contains(res.Contacted, "DNS: SRV records of example.org") {
		t.Errorf("contacted = %v", res.Contacted)
	}
	res, err = Discover(t.Context(), "me@starttls.example", true, f.network())
	if err != nil || res.IMAP.TLS != config.TLSStartTLS || res.IMAP.Port != 143 {
		t.Errorf("starttls srv = %+v, %v", res, err)
	}
	if _, err := Discover(t.Context(), "me@disabled.example", true, f.network()); !errors.Is(err, ErrNotFound) {
		t.Errorf("'.' target = %v", err)
	}
}

func TestMXHostedDomain(t *testing.T) {
	f := fake{mx: map[string][]*net.MX{"mycompany.example": {{Host: "mx01.posteo.de.", Pref: 10}}}}
	res, err := Discover(t.Context(), "me@mycompany.example", true, f.network())
	if err != nil || res.Source != SourceMX || res.IMAP.Host != "posteo.de" || res.Username != "me@mycompany.example" {
		t.Errorf("mx = %+v, %v", res, err)
	}
}

func TestOAuthOnlyIsReported(t *testing.T) {
	p := Provider{Name: "Big", IMAP: []Entry{{Host: "imap.big.example", Port: 993, Socket: "SSL", Auth: []string{"OAuth2"}}}}
	if _, err := fromProvider(p, SourceBuiltIn, "me", "big.example", nil); !errors.Is(err, ErrOAuthOnly) {
		t.Errorf("err = %v", err)
	}
}

func TestPlaintextIsNeverOffered(t *testing.T) {
	p := Provider{IMAP: []Entry{
		{Host: "plain.example.org", Port: 143, Socket: "plain"},
		{Host: "secure.example.org", Port: 993, Socket: "SSL", Username: "%EMAILLOCALPART%"},
	}, SMTP: []Entry{{Host: "smtp.example.org", Port: 25, Socket: "plain"}}}
	res, err := fromProvider(p, SourceBuiltIn, "me", "example.org", nil)
	if err != nil || res.IMAP.Host != "secure.example.org" || res.Username != "me" || res.SMTP != (config.Server{}) {
		t.Errorf("res = %+v, %v", res, err)
	}
	if _, err := fromProvider(Provider{IMAP: p.IMAP[:1]}, SourceBuiltIn, "me", "example.org", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("plaintext only = %v", err)
	}
}

const providerXML = `<?xml version="1.0"?>
<clientConfig version="1.1">
  <emailProvider id="example.net">
    <domain>example.net</domain>
    <displayName>Example Mail</displayName>
    <incomingServer type="pop3"><hostname>pop.example.net</hostname><port>995</port><socketType>SSL</socketType></incomingServer>
    <incomingServer type="imap">
      <hostname>imap.example.net</hostname><port>143</port><socketType>STARTTLS</socketType>
      <username>%EMAILADDRESS%</username><authentication>password-cleartext</authentication>
    </incomingServer>
    <outgoingServer type="smtp">
      <hostname>smtp.example.net</hostname><port>587</port><socketType>STARTTLS</socketType>
      <username>%EMAILADDRESS%</username><authentication>password-cleartext</authentication>
    </outgoingServer>
  </emailProvider>
</clientConfig>`

func TestProviderAutoconfigFile(t *testing.T) {
	var requested []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.String())
		if r.URL.Path == "/mail/config-v1.1.xml" {
			_, _ = w.Write([]byte(providerXML))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	client := srv.Client()
	inner := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		// Route every host to the test server.
		r = r.Clone(r.Context())
		r.URL.Host = strings.TrimPrefix(srv.URL, "https://")
		return inner.RoundTrip(r)
	})
	res, err := Discover(t.Context(), "me@example.net", true, fake{http: client}.network())
	if err != nil || res.Source != SourceProvider || res.IMAP != (config.Server{Host: "imap.example.net", Port: 143, TLS: "starttls"}) ||
		res.SMTP.Port != 587 || res.Provider != "Example Mail" {
		t.Fatalf("res = %+v, %v", res, err)
	}
	for _, u := range requested {
		if strings.Contains(u, "me@") || strings.Contains(u, "emailaddress") {
			t.Errorf("email address leaked in request %q", u)
		}
	}
}

func TestGuessNeedsVerifiedTLS(t *testing.T) {
	f := fake{probe: map[string]bool{"mail.guess.example:993": true, "smtp.guess.example:465": true}}
	res, err := Discover(t.Context(), "me@guess.example", true, f.network())
	if err != nil || res.Source != SourceGuess || res.IMAP.Host != "mail.guess.example" || res.SMTP.Host != "smtp.guess.example" {
		t.Fatalf("guess = %+v, %v", res, err)
	}
	res, err = Discover(t.Context(), "me@nothing.example", true, fake{}.network())
	if !errors.Is(err, ErrNotFound) || len(res.Contacted) == 0 {
		t.Errorf("nothing = %+v, %v", res, err)
	}
}

func TestBadEmail(t *testing.T) {
	for _, e := range []string{"", "no-at-sign", "Me <me@example.org>", "me@localhost", "me@exa mple.org"} {
		if _, err := Discover(t.Context(), e, false, offline(t)); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Discover(%q) = %v", e, err)
		}
	}
}

func TestParseXMLRejectsJunk(t *testing.T) {
	for _, doc := range []string{"", "<clientConfig/>", "not xml", strings.Repeat("<a>", maxXML)} {
		if _, err := ParseXML(strings.NewReader(doc)); err == nil {
			t.Errorf("ParseXML(%.20q) accepted", doc)
		}
	}
	p, err := ParseXML(strings.NewReader(strings.Replace(providerXML, "imap.example.net", "imap.example.net:143/../", 1)))
	if err != nil || len(p.IMAP) != 0 {
		t.Errorf("bad hostname kept: %+v, %v", p, err)
	}
}

func FuzzParseXML(f *testing.F) {
	f.Add(providerXML)
	f.Fuzz(func(t *testing.T, doc string) {
		p, err := ParseXML(strings.NewReader(doc))
		if err != nil {
			return
		}
		for _, e := range append(p.IMAP, p.SMTP...) {
			if !validHost(e.Host) || e.Port < 1 || e.Port > 65535 {
				t.Fatalf("invalid entry %+v", e)
			}
		}
	})
}

func TestValidHost(t *testing.T) {
	for _, h := range []string{"imap.example.org", "mx-1.mail.example.co.uk", "a.b"} {
		if !validHost(h) {
			t.Errorf("validHost(%q) = false", h)
		}
	}
	for _, h := range []string{"", "localhost", "10.0.0.1", "127.0.0.1", "a..b", "-.-", "-a.example", "a-.example",
		"exa mple.org", "example.org:993", "1.2.3", "imap.%EMAILDOMAIN%", strings.Repeat("a", 64) + ".org"} {
		if validHost(h) {
			t.Errorf("validHost(%q) = true", h)
		}
	}
}

func TestHostileUsernameIsReplaced(t *testing.T) {
	doc := strings.Replace(providerXML, "<username>%EMAILADDRESS%</username>",
		"<username>&#x9B;31mfake\nline</username>", 1)
	p, err := ParseXML(strings.NewReader(doc))
	if err != nil || len(p.IMAP) != 1 || p.IMAP[0].Username != "%EMAILADDRESS%" {
		t.Fatalf("username = %+v, %v", p.IMAP, err)
	}
	if got := expand("%EMAILLOCALPART% x", "me", "example.org"); got != "me@example.org" {
		t.Errorf("expand with a space = %q", got)
	}
}

func TestMXFallsThroughWhenUnusable(t *testing.T) {
	// The MX points to a provider whose IMAP is plaintext only; discovery
	// must go on to the guess.
	f := fake{
		mx:    map[string][]*net.MX{"hosted.example": {{Host: "mx.posteo.de."}}},
		probe: map[string]bool{"imap.hosted.example:993": true},
	}
	res, err := Discover(t.Context(), "me@hosted.example", true, f.network())
	if err != nil || res.Source != SourceMX {
		t.Fatalf("usable MX = %+v, %v", res, err)
	}
	f.mx = map[string][]*net.MX{"hosted.example": {{Host: "mx.nobody.example."}}}
	res, err = Discover(t.Context(), "me@hosted.example", true, f.network())
	if err != nil || res.Source != SourceGuess {
		t.Fatalf("fall-through = %+v, %v", res, err)
	}
}

func TestTrusted(t *testing.T) {
	in := func(host string, port int) config.Server {
		return config.Server{Host: host, Port: port, TLS: "implicit"}
	}
	tests := []struct {
		r    Result
		want bool
	}{
		{Result{Source: SourceBuiltIn, IMAP: in("imap.elsewhere.example", 993)}, true},
		{Result{Source: SourceSRV, IMAP: in("mail.example.org", 993)}, true},
		{Result{Source: SourceSRV, IMAP: in("mail.example.org", 6379)}, false},
		{Result{Source: SourceSRV, IMAP: in("imap.evil.example", 993)}, false},
		{Result{Source: SourceProvider, IMAP: in("example.org", 993), SMTP: in("smtp.evil.example", 465)}, false},
		{Result{Source: SourceGuess, IMAP: in("imap.example.org", 993), SMTP: in("smtp.example.org", 465)}, true},
		{Result{Source: SourceSRV, IMAP: in("notexample.org", 993)}, false},
	}
	for _, tt := range tests {
		if got := tt.r.Trusted("example.org"); got != tt.want {
			t.Errorf("Trusted(%+v) = %v, want %v", tt.r, got, tt.want)
		}
	}
}
