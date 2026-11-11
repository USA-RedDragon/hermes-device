package webpush

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

const (
	recordSize = 4096
	keyIDLen   = 65
	// MaxPayload is the largest payload Send accepts, in bytes.
	MaxPayload = 3000
)

// encrypt builds an aes128gcm body (RFC 8291, RFC 8188) with one record and
// no padding beyond the 0x02 delimiter.
func encrypt(uaPub *ecdh.PublicKey, auth, plaintext, salt []byte, asPriv *ecdh.PrivateKey) ([]byte, error) {
	if len(plaintext) > MaxPayload {
		return nil, fmt.Errorf("webpush: payload is %d bytes, limit %d", len(plaintext), MaxPayload)
	}
	if len(salt) != 16 {
		return nil, fmt.Errorf("webpush: salt is %d bytes, want 16", len(salt))
	}
	shared, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, fmt.Errorf("webpush: ecdh: %w", err)
	}
	asPub := asPriv.PublicKey().Bytes()
	uaBytes := uaPub.Bytes()

	keyInfo := make([]byte, 0, 14+65+65)
	keyInfo = append(keyInfo, "WebPush: info\x00"...)
	keyInfo = append(keyInfo, uaBytes...)
	keyInfo = append(keyInfo, asPub...)
	ikm, err := hkdf.Key(sha256.New, shared, auth, string(keyInfo), 32)
	if err != nil {
		return nil, fmt.Errorf("webpush: hkdf: %w", err)
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, fmt.Errorf("webpush: hkdf: %w", err)
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, fmt.Errorf("webpush: hkdf: %w", err)
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, fmt.Errorf("webpush: hkdf: %w", err)
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("webpush: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("webpush: gcm: %w", err)
	}

	out := make([]byte, 0, 16+4+1+len(asPub)+len(plaintext)+1+gcm.Overhead())
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, keyIDLen)
	out = append(out, asPub...)
	record := make([]byte, 0, len(plaintext)+1)
	record = append(record, plaintext...)
	record = append(record, 0x02)
	return gcm.Seal(out, nonce, record, nil), nil
}
