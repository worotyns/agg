// Package privacy removes obvious personal data from event properties before they are stored, as a safety net:
// developers decide what they send, but an email or a phone number pasted into props never reaches the database.
package privacy

import "strings"

// BaseBlockedFields are always removed, at any depth. Sites can add more.
var BaseBlockedFields = []string{
	"email", "e_mail", "phone", "phone_number", "first_name", "last_name", "full_name", "street", "address",
	"postal_code", "zip_code", "password", "card_number", "iban",
}

// Blocklist is a case-insensitive set of field names.
type Blocklist map[string]struct{}

func NewBlocklist(extra []string) Blocklist {
	b := Blocklist{}
	for _, f := range BaseBlockedFields {
		b[f] = struct{}{}
	}
	for _, f := range extra {
		if f = strings.ToLower(strings.TrimSpace(f)); f != "" {
			b[f] = struct{}{}
		}
	}
	return b
}

func (b Blocklist) Has(key string) bool {
	_, ok := b[strings.ToLower(key)]
	return ok
}

// Strip returns v with blocked keys removed from every nested object. Maps are modified in place.
func (b Blocklist) Strip(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if b.Has(k) {
				delete(t, k)
				continue
			}
			t[k] = b.Strip(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = b.Strip(t[i])
		}
		return t
	default:
		return v
	}
}
