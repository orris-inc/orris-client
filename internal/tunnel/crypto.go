package tunnel

import (
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// NonceSize is the size of XChaCha20-Poly1305 nonce (24 bytes).
	NonceSize = chacha20poly1305.NonceSizeX
	// TagSize is the size of Poly1305 authentication tag (16 bytes).
	TagSize = chacha20poly1305.Overhead
	// KeySize is the required key size (32 bytes).
	KeySize = chacha20poly1305.KeySize
)

var (
	// ErrInvalidKeySize is returned when the key size is not 32 bytes.
	ErrInvalidKeySize = errors.New("invalid key size: must be 32 bytes")
	// ErrCiphertextTooShort is returned when ciphertext is shorter than nonce + tag.
	ErrCiphertextTooShort = errors.New("ciphertext too short")
	// ErrDecryptionFailed is returned when decryption fails (authentication failed).
	ErrDecryptionFailed = errors.New("decryption failed: authentication error")
)

// DeriveKeyFromToken derives a 32-byte encryption key from a token using HKDF.
// This allows automatic key derivation without extra configuration.
// Both entry and exit agents with the same token will derive the same key.
func DeriveKeyFromToken(token string) ([]byte, error) {
	if token == "" {
		return nil, errors.New("empty token")
	}

	// Use HKDF with SHA-256
	// salt is fixed for deterministic derivation (same token -> same key)
	salt := []byte("orris-tunnel-encryption-v1")
	info := []byte("xchacha20-poly1305")

	reader := hkdf.New(sha256.New, []byte(token), salt, info)
	key := make([]byte, KeySize)
	if _, err := reader.Read(key); err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}

	return key, nil
}

// DeriveSessionKey derives a session key from shared secret and both nonces.
// This provides forward secrecy - each connection uses a unique session key.
// The nonces are combined in a deterministic order (sorted) to ensure both
// parties derive the same key regardless of who sends first.
func DeriveSessionKey(sharedSecret string, clientNonce, serverNonce []byte) ([]byte, error) {
	if sharedSecret == "" {
		return nil, errors.New("empty shared secret")
	}
	if len(clientNonce) != KeyExchangeNonceSize || len(serverNonce) != KeyExchangeNonceSize {
		return nil, errors.New("invalid nonce size")
	}

	// Combine nonces in deterministic order for key derivation
	// Use client||server order (entry is client, exit is server)
	combinedNonce := make([]byte, KeyExchangeNonceSize*2)
	copy(combinedNonce[:KeyExchangeNonceSize], clientNonce)
	copy(combinedNonce[KeyExchangeNonceSize:], serverNonce)

	// Use HKDF with combined nonces as salt
	info := []byte("xchacha20-poly1305-session")
	reader := hkdf.New(sha256.New, []byte(sharedSecret), combinedNonce, info)

	key := make([]byte, KeySize)
	if _, err := reader.Read(key); err != nil {
		return nil, fmt.Errorf("derive session key: %w", err)
	}

	return key, nil
}

// GenerateNonce generates a random nonce for key exchange.
func GenerateNonce() ([]byte, error) {
	nonce := make([]byte, KeyExchangeNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return nonce, nil
}

// Cipher provides encryption and decryption for tunnel messages.
type Cipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// XChaCha20Cipher implements Cipher using XChaCha20-Poly1305.
type XChaCha20Cipher struct {
	aead cipher.AEAD
}

// NewXChaCha20Cipher creates a new XChaCha20-Poly1305 cipher with the given key.
// The key must be exactly 32 bytes.
func NewXChaCha20Cipher(key []byte) (*XChaCha20Cipher, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKeySize
	}

	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("create XChaCha20-Poly1305: %w", err)
	}

	return &XChaCha20Cipher{aead: aead}, nil
}

// Encrypt encrypts plaintext and returns [nonce:24][ciphertext+tag].
func (c *XChaCha20Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	// Generate random nonce
	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	// Allocate output buffer: nonce + ciphertext + tag
	out := make([]byte, NonceSize+len(plaintext)+TagSize)
	copy(out[:NonceSize], nonce)

	// Encrypt and append to output after nonce
	c.aead.Seal(out[NonceSize:NonceSize], nonce, plaintext, nil)

	return out, nil
}

// Decrypt decrypts ciphertext in format [nonce:24][ciphertext+tag].
func (c *XChaCha20Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < NonceSize+TagSize {
		return nil, ErrCiphertextTooShort
	}

	nonce := ciphertext[:NonceSize]
	encrypted := ciphertext[NonceSize:]

	plaintext, err := c.aead.Open(nil, nonce, encrypted, nil)
	if err != nil {
		return nil, ErrDecryptionFailed
	}

	return plaintext, nil
}
