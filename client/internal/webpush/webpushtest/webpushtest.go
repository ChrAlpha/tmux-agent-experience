// Package webpushtest is a browser's end of Web Push, for tests: it holds a
// subscription's private key and reads the messages pushed to it, following
// RFC 8291 from the receiving side.
package webpushtest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
)

// Receiver is one subscription's private half.
type Receiver struct {
	key    *ecdh.PrivateKey
	secret []byte
}

// New returns a receiver and its subscription's public keys, base64url as
// browsers give them.
func New() (r *Receiver, p256dh, auth string) {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	b64 := base64.RawURLEncoding
	return &Receiver{k, secret}, b64.EncodeToString(k.PublicKey().Bytes()), b64.EncodeToString(secret)
}

// Open decrypts an aes128gcm message body.
func (r *Receiver) Open(body []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, errors.New("no aes128gcm header")
	}
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if rs != 4096 || idlen != 65 || len(body) < 21+idlen+17 {
		return nil, fmt.Errorf("record size %d, key id length %d", rs, idlen)
	}
	keyID := body[21 : 21+idlen]
	sender, err := ecdh.P256().NewPublicKey(keyID)
	if err != nil {
		return nil, err
	}
	shared, err := r.key.ECDH(sender)
	if err != nil {
		return nil, err
	}
	info := append(append([]byte("WebPush: info\x00"), r.key.PublicKey().Bytes()...), keyID...)
	ikm, err := hkdf.Key(sha256.New, shared, r.secret, string(info), 32)
	if err != nil {
		return nil, err
	}
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		return nil, err
	}
	if len(plain) == 0 || plain[len(plain)-1] != 2 {
		return nil, errors.New("no last-record delimiter")
	}
	return plain[:len(plain)-1], nil
}
