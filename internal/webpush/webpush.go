// Package webpush sends Web Push notifications: payload encryption (RFC 8291, aes128gcm per RFC 8188)
// and VAPID authentication (RFC 8292), using only the standard library.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var b64 = base64.RawURLEncoding

// decodeB64 accepts base64url with or without padding (browsers and libraries differ).
func decodeB64(s string) ([]byte, error) {
	s = strings.TrimRight(strings.NewReplacer("+", "-", "/", "_").Replace(strings.TrimSpace(s)), "=")
	return b64.DecodeString(s)
}

// VAPID is the application server key pair. PublicKey is the uncompressed P-256 point, base64url,
// as passed to pushManager.subscribe({ applicationServerKey }).
type VAPID struct {
	PrivateKey string `json:"privateKey"`
	PublicKey  string `json:"publicKey"`
	Subject    string `json:"-"`
	key        *ecdsa.PrivateKey
}

// GenerateVAPID creates a new key pair.
func GenerateVAPID() (*VAPID, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	pub, err := k.PublicKey.ECDH()
	if err != nil {
		return nil, err
	}
	d := make([]byte, 32)
	k.D.FillBytes(d)
	return &VAPID{PrivateKey: b64.EncodeToString(d), PublicKey: b64.EncodeToString(pub.Bytes()), key: k}, nil
}

// Load restores the private key after JSON decoding.
func (v *VAPID) Load() error {
	d, err := decodeB64(v.PrivateKey)
	if err != nil {
		return err
	}
	priv, err := ecdh.P256().NewPrivateKey(d)
	if err != nil {
		return err
	}
	pub := priv.PublicKey().Bytes()
	x, y := new(big.Int).SetBytes(pub[1:33]), new(big.Int).SetBytes(pub[33:])
	v.key = &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, D: new(big.Int).SetBytes(d)}
	v.PublicKey = b64.EncodeToString(pub)
	return nil
}

// JWT returns the VAPID token for a push service origin (aud), valid for 12 hours.
func (v *VAPID) JWT(aud string, now time.Time) (string, error) {
	if v.key == nil {
		if err := v.Load(); err != nil {
			return "", err
		}
	}
	sub := v.Subject
	if sub == "" {
		sub = "mailto:admin@example.com"
	}
	header := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{"aud": aud, "exp": now.Add(12 * time.Hour).Unix(), "sub": sub})
	signing := header + "." + b64.EncodeToString(claims)
	h := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, v.key, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64.EncodeToString(sig), nil
}

// Subscription is what the browser returns from pushManager.subscribe().
type Subscription struct {
	Endpoint string `json:"endpoint"`
	P256dh   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

const recordSize = 4096

// Encrypt encrypts a payload for a subscription (RFC 8291). salt and asPriv may be nil (random).
func Encrypt(sub Subscription, payload []byte, salt []byte, asPriv *ecdh.PrivateKey) ([]byte, error) {
	uaPubBytes, err := decodeB64(sub.P256dh)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	authSecret, err := decodeB64(sub.Auth)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	if len(authSecret) != 16 {
		return nil, errors.New("auth secret must be 16 bytes")
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaPubBytes)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	if asPriv == nil {
		if asPriv, err = ecdh.P256().GenerateKey(rand.Reader); err != nil {
			return nil, err
		}
	}
	if salt == nil {
		salt = make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
	}
	asPub := asPriv.PublicKey().Bytes()
	secret, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	keyInfo := "WebPush: info\x00" + string(uaPubBytes) + string(asPub)
	ikm, err := hkdf.Key(sha256.New, secret, authSecret, keyInfo, 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	if len(payload)+1+16 > recordSize {
		return nil, errors.New("payload too large")
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain := append(append([]byte{}, payload...), 0x02) // delimiter of the last (only) record
	var out bytes.Buffer
	out.Write(salt)
	binary.Write(&out, binary.BigEndian, uint32(recordSize))
	out.WriteByte(byte(len(asPub)))
	out.Write(asPub)
	out.Write(gcm.Seal(nil, nonce, plain, nil))
	return out.Bytes(), nil
}

// ErrGone means the subscription expired or was removed; delete it.
var ErrGone = errors.New("subscription is gone")

// Message is the JSON the service worker receives.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url,omitempty"`
	Tag   string `json:"tag,omitempty"`
}

// Sender delivers messages to push services.
type Sender struct {
	VAPID  *VAPID
	Client *http.Client
	Now    func() time.Time
}

// Send encrypts and posts one message to one subscription.
func (s *Sender) Send(ctx context.Context, sub Subscription, msg Message) error {
	payload, _ := json.Marshal(msg)
	body, err := Encrypt(sub, payload, nil, nil)
	if err != nil {
		return err
	}
	u, err := url.Parse(sub.Endpoint)
	if err != nil || u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("invalid endpoint")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	jwt, err := s.VAPID.JWT(u.Scheme+"://"+u.Host, now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", strconv.Itoa(24*3600))
	req.Header.Set("Urgency", "high")
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+s.VAPID.PublicKey)
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	msgBody, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	switch {
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone:
		return ErrGone
	case res.StatusCode >= 300:
		return fmt.Errorf("push service: %s %s", res.Status, strings.TrimSpace(string(msgBody)))
	}
	return nil
}
