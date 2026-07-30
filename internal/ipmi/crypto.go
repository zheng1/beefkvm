package ipmi

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"errors"
	"fmt"
)

// hmacSHA1 is a thin wrapper so call sites read cleanly.
func hmacSHA1(key, data []byte) []byte {
	m := hmac.New(sha1.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// aesEncryptPayload PKCS-style-pads the plaintext per IPMI 2.0 §13.29
// (pad bytes 01, 02, 03, ..., padLen followed by a byte holding padLen),
// then encrypts with AES-CBC-128. The IV must be 16 random bytes.
func aesEncryptPayload(key, iv, plain []byte) []byte {
	// Pad so that (len(plain) + padLen + 1) % 16 == 0.
	padLen := 16 - ((len(plain) + 1) % 16)
	if padLen == 16 {
		padLen = 0
	}
	out := make([]byte, 0, len(plain)+padLen+1)
	out = append(out, plain...)
	for i := 1; i <= padLen; i++ {
		out = append(out, byte(i))
	}
	out = append(out, byte(padLen))

	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // 16-byte key, cannot fail
	}
	mode := cipher.NewCBCEncrypter(block, iv)
	ct := make([]byte, len(out))
	mode.CryptBlocks(ct, out)
	return ct
}

// aesDecryptPayload reverses aesEncryptPayload: strips the pad-length byte
// and the trailing 01..N pad. Refuses to interpret malformed padding.
func aesDecryptPayload(key, iv, ct []byte) ([]byte, error) {
	if len(ct)%16 != 0 || len(ct) == 0 {
		return nil, errors.New("ipmi: ciphertext length not aligned")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	padLen := int(pt[len(pt)-1])
	if padLen > 15 || padLen+1 > len(pt) {
		return nil, fmt.Errorf("ipmi: bad AES pad length %d", padLen)
	}
	// Verify pad bytes.
	for i := 0; i < padLen; i++ {
		if pt[len(pt)-2-i] != byte(padLen-i) {
			return nil, errors.New("ipmi: bad AES pad content")
		}
	}
	return pt[:len(pt)-padLen-1], nil
}

// newIV returns 16 cryptographically random bytes for AES-CBC.
func newIV() []byte {
	iv := make([]byte, 16)
	if _, err := rand.Read(iv); err != nil {
		panic(err)
	}
	return iv
}
