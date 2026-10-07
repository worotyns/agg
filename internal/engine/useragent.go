package engine

import (
	"regexp"
	"strings"
)

// UserAgent is the parsed part of a User-Agent header that agg keeps: no version numbers, no raw string.
type UserAgent struct {
	Browser string
	OS      string
	Device  string // desktop, mobile, tablet
	Bot     bool
}

var botRe = regexp.MustCompile(`(?i)bot\b|bot/|crawl|spider|slurp|headless|lighthouse|pagespeed|preview|monitor|uptime|curl/|wget|python-|go-http-client|java/|okhttp|axios|node-fetch|scrapy|phantomjs|facebookexternalhit|embedly|whatsapp`)

// ParseUserAgent recognises the common browsers and systems. Anything else is "Other".
func ParseUserAgent(ua string) UserAgent {
	if strings.TrimSpace(ua) == "" {
		return UserAgent{} // e.g. a server-side call without a User-Agent
	}
	u := UserAgent{Browser: "Other", OS: "Other", Device: "desktop", Bot: botRe.MatchString(ua)}
	has := func(s string) bool { return strings.Contains(ua, s) }
	switch {
	case has("Edg/") || has("EdgA/") || has("EdgiOS/"):
		u.Browser = "Edge"
	case has("OPR/") || has("Opera"):
		u.Browser = "Opera"
	case has("SamsungBrowser/"):
		u.Browser = "Samsung Internet"
	case has("Firefox/") || has("FxiOS/"):
		u.Browser = "Firefox"
	case has("Chrome/") || has("CriOS/") || has("Chromium/"):
		u.Browser = "Chrome"
	case has("Safari/") && has("Version/"):
		u.Browser = "Safari"
	}
	switch {
	case has("iPhone") || has("iPod"):
		u.OS, u.Device = "iOS", "mobile"
	case has("iPad"):
		u.OS, u.Device = "iOS", "tablet"
	case has("Android"):
		u.OS = "Android"
		u.Device = "tablet"
		if has("Mobile") {
			u.Device = "mobile"
		}
	case has("Windows"):
		u.OS = "Windows"
	case has("CrOS"):
		u.OS = "ChromeOS"
	case has("Mac OS X") || has("Macintosh"):
		u.OS = "macOS"
	case has("Linux"):
		u.OS = "Linux"
	}
	return u
}
