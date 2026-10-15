// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"net/url"
	"slices"
	"strings"
)

// trackingParams are query parameters that only identify the reader or the
// campaign. Names ending in "_" match as prefixes.
var trackingParams = []string{
	"utm_", "fbclid", "gclid", "gclsrc", "dclid", "msclkid", "yclid", "igshid", "twclid", "ttclid",
	"mc_eid", "mc_cid", "_hsenc", "_hsmi", "__hssc", "__hstc", "__hsfp", "hsctatracking",
	"mkt_tok", "oly_enc_id", "oly_anon_id", "vero_id", "vero_conv", "_ke", "ck_subscriber_id",
	"wickedid", "rb_clickid", "s_cid", "sc_cid", "spm", "trk", "trkcampaign", "ref_src", "ref_url",
	"elqtrack", "elqtrackid", "elq", "elqaid", "elqat", "elqcampaignid", "mbid", "cmpid", "sfmc_id",
	"triedredirect", "_bta_tid", "_bta_c", "_branch_match_id", "si", "feature", "pp",
}

// redirectParams may hold the real destination of a redirect link.
var redirectParams = []string{"url", "u", "q", "target", "dest", "destination", "redirect", "redirect_url",
	"redirect_uri", "to", "link", "goto", "out", "r", "rurl"}

// redirectHosts hint at click-tracking services whose links hide the
// destination.
var redirectHosts = []string{
	"click.", "clicks.", "links.", "link.", "track.", "tracking.", "trk.", "email.", "e.", "t.", "url.",
	"list-manage.com", "sendgrid.net", "mailgun", "mailchi.mp", "hubspotlinks", "hs-sites", "mandrillapp",
	"substack.com/redirect", "mg-", "ct.", "go.", "r.",
}

// Cleaned is a link with tracking removed.
type Cleaned struct {
	URL       string   `json:"url"`
	Removed   []string `json:"removed,omitempty"`   // tracking parameters dropped
	Unwrapped string   `json:"unwrapped,omitempty"` // the redirect service skipped, if any
	// Opaque means the link goes through a tracking redirect whose real
	// destination is not visible, so it cannot be cleaned.
	Opaque bool `json:"opaque,omitempty"`
}

// Changed reports whether cleaning changed the link.
func (c Cleaned) Changed(original string) bool { return c.URL != original }

// CleanURL removes tracking from a link, locally: known tracking
// parameters are dropped and redirect links that carry their destination
// are unwrapped (up to a few levels). Only http(s) results are returned;
// anything else leaves the link as it was.
func CleanURL(raw string) Cleaned {
	out := Cleaned{URL: raw}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return out
	}
	for range 3 {
		dest, ok := redirectTarget(u)
		if !ok {
			break
		}
		if out.Unwrapped == "" {
			out.Unwrapped = u.Hostname()
		}
		u = dest
	}
	q := u.Query()
	for name := range q {
		if isTracking(name) {
			out.Removed = append(out.Removed, name)
			q.Del(name)
		}
	}
	slices.Sort(out.Removed)
	if len(out.Removed) > 0 {
		u.RawQuery = q.Encode() // only rewritten when something was removed
	}
	if strings.HasPrefix(u.Fragment, "utm_") || strings.Contains(u.Fragment, "&utm_") {
		u.Fragment = ""
	}
	cleaned := u.String()
	if check, err := url.Parse(cleaned); err != nil || check.Host == "" ||
		(check.Scheme != "http" && check.Scheme != "https") {
		return Cleaned{URL: raw} // never return something worse than the input
	}
	out.URL = cleaned
	out.Opaque = out.Unwrapped == "" && looksLikeRedirect(u)
	return out
}

func isTracking(name string) bool {
	n := strings.ToLower(name)
	for _, p := range trackingParams {
		if strings.HasSuffix(p, "_") && strings.HasPrefix(n, p) || n == p {
			return true
		}
	}
	return false
}

// redirectTarget returns the destination carried in a redirect link's
// query, if one parameter holds a full http(s) URL.
func redirectTarget(u *url.URL) (*url.URL, bool) {
	q := u.Query()
	for _, name := range redirectParams {
		v := q.Get(name)
		if v == "" {
			continue
		}
		dest, err := url.Parse(v)
		if err == nil && (dest.Scheme == "http" || dest.Scheme == "https") && dest.Host != "" {
			return dest, true
		}
	}
	return nil, false
}

func looksLikeRedirect(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	full := host + u.Path
	for _, h := range redirectHosts {
		// "click." and the like name a subdomain: match at the start only.
		match := strings.HasPrefix(host, h)
		if !strings.HasSuffix(h, ".") {
			match = match || strings.Contains(full, h)
		}
		if match {
			return len(u.Path) > 20 || u.RawQuery != ""
		}
	}
	return false
}
