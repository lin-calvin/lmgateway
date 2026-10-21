// Package keytag parses the non-authenticating metadata a client may attach to
// a request: an attribution tag and a small set of route hints, written as a
// URL-query-like suffix on the API key.
//
//	Authorization: Bearer sk-...#claude-code&pool_policy=price_first
//
// The suffix is split at the first '#'. The first bare segment is the
// attribution tag; every "key=value" segment is a route hint. Tags never
// participate in key lookup, groups or ACL.
package keytag

import (
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// TagField is the request-document field that carries the attribution tag.
	TagField = "key_tag"
	// HintField is the canonical query string of the route hints.
	HintField = "route_hint"
	// HintsField is the normalized map of route hints.
	HintsField = "route_hints"

	MaxTagLen   = 128
	MaxKeyLen   = 64
	MaxValueLen = 128
	MaxHints    = 16
	MaxSuffix   = 1024
)

// Meta is the parsed key suffix.
type Meta struct {
	Tag   string            // first bare segment, empty when absent
	Hints map[string]string // key=value segments, lowercased and deduped
}

// SplitToken separates an optional "#suffix" from a presented token. ok is false
// when there is no suffix or the base is empty. The suffix is not validated
// here: it never participates in authentication.
func SplitToken(token string) (base, suffix string, ok bool) {
	i := strings.IndexByte(token, '#')
	if i <= 0 {
		return token, "", false
	}
	return token[:i], token[i+1:], true
}

// ParseSuffix parses the text after '#' (without the '#'). It is deliberately
// permissive: malformed segments are dropped rather than failing the request.
func ParseSuffix(raw string) Meta {
	meta := Meta{}
	if raw == "" || len(raw) > MaxSuffix {
		return meta
	}
	for _, segment := range strings.Split(raw, "&") {
		if segment == "" {
			continue
		}
		if i := strings.IndexByte(segment, '='); i >= 0 {
			key, ok := unescape(segment[:i])
			if !ok {
				continue
			}
			value, ok := unescape(segment[i+1:])
			if !ok {
				continue
			}
			key = normalize(key)
			value = normalize(value)
			if !validPart(key, MaxKeyLen) || !validPart(value, MaxValueLen) {
				continue
			}
			if meta.Hints == nil {
				meta.Hints = map[string]string{}
			}
			if _, exists := meta.Hints[key]; !exists && len(meta.Hints) >= MaxHints {
				continue
			}
			meta.Hints[key] = value // duplicate keys: last wins
			continue
		}
		if meta.Tag != "" {
			continue
		}
		value, ok := unescape(segment)
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if ValidTag(value) {
			meta.Tag = value
		}
	}
	return meta
}

// ParseQuery parses only "key=value" segments; bare segments are ignored. It is
// used for a body route_hint string.
func ParseQuery(raw string) map[string]string {
	return ParseSuffix(raw).Hints
}

// NormalizeMap validates and normalizes a map supplied in the request body.
func NormalizeMap(in map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		s, ok := v.(string)
		if !ok {
			continue
		}
		key := normalize(k)
		value := normalize(s)
		if !validPart(key, MaxKeyLen) || !validPart(value, MaxValueLen) {
			continue
		}
		if _, exists := out[key]; !exists && len(out) >= MaxHints {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Canonical returns a stable query string: keys sorted and URL-encoded.
func Canonical(hints map[string]string) string {
	if len(hints) == 0 {
		return ""
	}
	keys := make([]string, 0, len(hints))
	for k := range hints {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(hints[k]))
	}
	return strings.Join(parts, "&")
}

// ValidTag reports whether a decoded value may be used as an attribution tag.
func ValidTag(tag string) bool {
	return validPart(tag, MaxTagLen)
}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func validPart(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func unescape(s string) (string, bool) {
	v, err := url.QueryUnescape(s)
	return v, err == nil
}
