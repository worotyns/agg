package webpush

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	b, err := decodeB64(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The example from RFC 8291, section 5.
func TestEncryptMatchesRFC8291Example(t *testing.T) {
	asPriv, err := ecdh.P256().NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	sub := Subscription{
		P256dh: "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
		Auth:   "BTBZMqHH6r4Tts7J_aSIgg",
	}
	got, err := Encrypt(sub, []byte("When I grow up, I want to be a watermelon"), mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw"), asPriv)
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if b64.EncodeToString(got) != want {
		t.Errorf("ciphertext differs from RFC 8291\n got %s\nwant %s", b64.EncodeToString(got), want)
	}
}

// decrypt is an independent implementation of the user agent side, used to check round trips.
func decrypt(t *testing.T, uaPriv *ecdh.PrivateKey, auth, body []byte) []byte {
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if rs != 4096 || idlen != 65 {
		t.Fatalf("header rs=%d idlen=%d", rs, idlen)
	}
	asPub, err := ecdh.P256().NewPublicKey(body[21 : 21+idlen])
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := uaPriv.ECDH(asPub)
	info := "WebPush: info\x00" + string(uaPriv.PublicKey().Bytes()) + string(asPub.Bytes())
	ikm, _ := hkdf.Key(sha256.New, secret, auth, info, 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if plain[len(plain)-1] != 0x02 {
		t.Fatal("missing last-record delimiter")
	}
	return plain[:len(plain)-1]
}

func newBrowser(t *testing.T) (*ecdh.PrivateKey, []byte, Subscription) {
	uaPriv, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	return uaPriv, auth, Subscription{P256dh: b64.EncodeToString(uaPriv.PublicKey().Bytes()), Auth: b64.EncodeToString(auth)}
}

func TestSendDeliversDecryptableMessageWithValidVAPID(t *testing.T) {
	uaPriv, auth, sub := newBrowser(t)
	vapid, err := GenerateVAPID()
	if err != nil {
		t.Fatal(err)
	}
	vapid.Subject = "mailto:ops@example.com"
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, body = r, must(io.ReadAll(r.Body))
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	sub.Endpoint = srv.URL + "/push/abc"
	now := time.Unix(1_800_000_000, 0)
	s := &Sender{VAPID: vapid, Now: func() time.Time { return now }}
	if err := s.Send(context.Background(), sub, Message{Title: "Alert: no orders", Body: "orders_1h = 0", URL: "https://agg.example.com/"}); err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("Content-Encoding") != "aes128gcm" || got.Header.Get("TTL") == "" {
		t.Errorf("headers: %v", got.Header)
	}
	var msg Message
	if err := json.Unmarshal(decrypt(t, uaPriv, auth, body), &msg); err != nil || msg.Title != "Alert: no orders" {
		t.Errorf("payload %+v %v", msg, err)
	}

	// Authorization: vapid t=<jwt>, k=<public key>
	a := got.Header.Get("Authorization")
	parts := strings.SplitN(strings.TrimPrefix(a, "vapid "), ", ", 2)
	jwt, k := strings.TrimPrefix(parts[0], "t="), strings.TrimPrefix(parts[1], "k=")
	if k != vapid.PublicKey {
		t.Error("k is not the VAPID public key")
	}
	seg := strings.Split(jwt, ".")
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	json.Unmarshal(mustB64(t, seg[1]), &claims)
	if claims.Aud != srv.URL || claims.Sub != "mailto:ops@example.com" || claims.Exp != now.Add(12*time.Hour).Unix() {
		t.Errorf("claims %+v", claims)
	}
	pub := mustB64(t, k)
	key := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(pub[1:33]), Y: new(big.Int).SetBytes(pub[33:])}
	sig := mustB64(t, seg[2])
	h := sha256.Sum256([]byte(seg[0] + "." + seg[1]))
	if !ecdsa.Verify(key, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Error("VAPID signature does not verify")
	}
}

func TestVAPIDSurvivesJSONRoundTrip(t *testing.T) {
	v, _ := GenerateVAPID()
	b, _ := json.Marshal(v)
	var w VAPID
	json.Unmarshal(b, &w)
	if err := w.Load(); err != nil || w.PublicKey != v.PublicKey {
		t.Fatalf("reload: %v %s %s", err, w.PublicKey, v.PublicKey)
	}
	if _, err := w.JWT("https://push.example", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestGoneSubscription(t *testing.T) {
	_, _, sub := newBrowser(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusGone) }))
	defer srv.Close()
	sub.Endpoint = srv.URL
	v, _ := GenerateVAPID()
	if err := (&Sender{VAPID: v}).Send(context.Background(), sub, Message{Title: "x"}); err != ErrGone {
		t.Errorf("err = %v, want ErrGone", err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
