package tunnel

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestNewXChaCha20Cipher(t *testing.T) {
	// Test valid key
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	cipher, err := NewXChaCha20Cipher(key)
	if err != nil {
		t.Fatalf("NewXChaCha20Cipher failed: %v", err)
	}
	if cipher == nil {
		t.Fatal("cipher is nil")
	}

	// Test invalid key sizes
	testCases := []struct {
		name    string
		keyLen  int
		wantErr error
	}{
		{"empty key", 0, ErrInvalidKeySize},
		{"short key", 16, ErrInvalidKeySize},
		{"long key", 64, ErrInvalidKeySize},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			invalidKey := make([]byte, tc.keyLen)
			_, err := NewXChaCha20Cipher(invalidKey)
			if err != tc.wantErr {
				t.Errorf("expected error %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestEncryptDecrypt(t *testing.T) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	cipher, err := NewXChaCha20Cipher(key)
	if err != nil {
		t.Fatalf("NewXChaCha20Cipher failed: %v", err)
	}

	testCases := []struct {
		name      string
		plaintext []byte
	}{
		{"empty", []byte{}},
		{"short", []byte("hello")},
		{"medium", []byte("hello world, this is a test message for encryption")},
		{"binary", []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0xfd}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ciphertext, err := cipher.Encrypt(tc.plaintext)
			if err != nil {
				t.Fatalf("Encrypt failed: %v", err)
			}

			// Check ciphertext length: nonce + plaintext + tag
			expectedLen := NonceSize + len(tc.plaintext) + TagSize
			if len(ciphertext) != expectedLen {
				t.Errorf("ciphertext length = %d, want %d", len(ciphertext), expectedLen)
			}

			decrypted, err := cipher.Decrypt(ciphertext)
			if err != nil {
				t.Fatalf("Decrypt failed: %v", err)
			}

			if !bytes.Equal(decrypted, tc.plaintext) {
				t.Errorf("decrypted = %v, want %v", decrypted, tc.plaintext)
			}
		})
	}
}

func TestEncryptDifferentNonces(t *testing.T) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	cipher, err := NewXChaCha20Cipher(key)
	if err != nil {
		t.Fatalf("NewXChaCha20Cipher failed: %v", err)
	}

	plaintext := []byte("test message")

	// Encrypt same message twice
	ct1, err := cipher.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	ct2, err := cipher.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	// Nonces should be different (ciphertexts should be different)
	if bytes.Equal(ct1, ct2) {
		t.Error("two encryptions of same plaintext produced identical ciphertext")
	}

	// But both should decrypt to same plaintext
	dec1, err := cipher.Decrypt(ct1)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	dec2, err := cipher.Decrypt(ct2)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if !bytes.Equal(dec1, plaintext) || !bytes.Equal(dec2, plaintext) {
		t.Error("decrypted plaintexts don't match original")
	}
}

func TestDecryptErrors(t *testing.T) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	cipher, err := NewXChaCha20Cipher(key)
	if err != nil {
		t.Fatalf("NewXChaCha20Cipher failed: %v", err)
	}

	// Test ciphertext too short
	shortCiphertext := make([]byte, NonceSize+TagSize-1)
	_, err = cipher.Decrypt(shortCiphertext)
	if err != ErrCiphertextTooShort {
		t.Errorf("expected ErrCiphertextTooShort, got %v", err)
	}

	// Test authentication failure (tampered ciphertext)
	plaintext := []byte("test message")
	ciphertext, err := cipher.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	// Tamper with ciphertext
	ciphertext[len(ciphertext)-1] ^= 0xff

	_, err = cipher.Decrypt(ciphertext)
	if err != ErrDecryptionFailed {
		t.Errorf("expected ErrDecryptionFailed, got %v", err)
	}
}

func TestDifferentKeys(t *testing.T) {
	key1 := make([]byte, KeySize)
	key2 := make([]byte, KeySize)
	if _, err := rand.Read(key1); err != nil {
		t.Fatalf("failed to generate key1: %v", err)
	}
	if _, err := rand.Read(key2); err != nil {
		t.Fatalf("failed to generate key2: %v", err)
	}

	cipher1, err := NewXChaCha20Cipher(key1)
	if err != nil {
		t.Fatalf("NewXChaCha20Cipher failed: %v", err)
	}

	cipher2, err := NewXChaCha20Cipher(key2)
	if err != nil {
		t.Fatalf("NewXChaCha20Cipher failed: %v", err)
	}

	plaintext := []byte("secret message")
	ciphertext, err := cipher1.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	// Decryption with wrong key should fail
	_, err = cipher2.Decrypt(ciphertext)
	if err != ErrDecryptionFailed {
		t.Errorf("expected ErrDecryptionFailed when using wrong key, got %v", err)
	}
}

func TestDeriveKeyFromToken(t *testing.T) {
	// Test empty token
	_, err := DeriveKeyFromToken("")
	if err == nil {
		t.Error("expected error for empty token")
	}

	// Test valid token
	token := "fwd_abc123_signature"
	key, err := DeriveKeyFromToken(token)
	if err != nil {
		t.Fatalf("DeriveKeyFromToken failed: %v", err)
	}
	if len(key) != KeySize {
		t.Errorf("key length = %d, want %d", len(key), KeySize)
	}

	// Test deterministic derivation (same token -> same key)
	key2, err := DeriveKeyFromToken(token)
	if err != nil {
		t.Fatalf("DeriveKeyFromToken failed: %v", err)
	}
	if !bytes.Equal(key, key2) {
		t.Error("same token should derive same key")
	}

	// Test different tokens -> different keys
	key3, err := DeriveKeyFromToken("different_token")
	if err != nil {
		t.Fatalf("DeriveKeyFromToken failed: %v", err)
	}
	if bytes.Equal(key, key3) {
		t.Error("different tokens should derive different keys")
	}
}

func TestGenerateNonce(t *testing.T) {
	// Test nonce generation
	nonce1, err := GenerateNonce()
	if err != nil {
		t.Fatalf("GenerateNonce failed: %v", err)
	}
	if len(nonce1) != KeyExchangeNonceSize {
		t.Errorf("nonce length = %d, want %d", len(nonce1), KeyExchangeNonceSize)
	}

	// Test uniqueness (two nonces should be different)
	nonce2, err := GenerateNonce()
	if err != nil {
		t.Fatalf("GenerateNonce failed: %v", err)
	}
	if bytes.Equal(nonce1, nonce2) {
		t.Error("two generated nonces should be different")
	}
}

func TestDeriveSessionKey(t *testing.T) {
	sharedSecret := "test-shared-secret"
	clientNonce, _ := GenerateNonce()
	serverNonce, _ := GenerateNonce()

	// Test successful derivation
	key, err := DeriveSessionKey(sharedSecret, clientNonce, serverNonce)
	if err != nil {
		t.Fatalf("DeriveSessionKey failed: %v", err)
	}
	if len(key) != KeySize {
		t.Errorf("key length = %d, want %d", len(key), KeySize)
	}

	// Test deterministic derivation (same inputs -> same key)
	key2, err := DeriveSessionKey(sharedSecret, clientNonce, serverNonce)
	if err != nil {
		t.Fatalf("DeriveSessionKey failed: %v", err)
	}
	if !bytes.Equal(key, key2) {
		t.Error("same inputs should derive same key")
	}

	// Test different nonces -> different keys
	differentNonce, _ := GenerateNonce()
	key3, err := DeriveSessionKey(sharedSecret, clientNonce, differentNonce)
	if err != nil {
		t.Fatalf("DeriveSessionKey failed: %v", err)
	}
	if bytes.Equal(key, key3) {
		t.Error("different nonces should derive different keys")
	}

	// Test different shared secret -> different keys
	key4, err := DeriveSessionKey("different-secret", clientNonce, serverNonce)
	if err != nil {
		t.Fatalf("DeriveSessionKey failed: %v", err)
	}
	if bytes.Equal(key, key4) {
		t.Error("different shared secrets should derive different keys")
	}

	// Test error cases
	_, err = DeriveSessionKey("", clientNonce, serverNonce)
	if err == nil {
		t.Error("expected error for empty shared secret")
	}

	_, err = DeriveSessionKey(sharedSecret, []byte("short"), serverNonce)
	if err == nil {
		t.Error("expected error for invalid client nonce size")
	}

	_, err = DeriveSessionKey(sharedSecret, clientNonce, []byte("short"))
	if err == nil {
		t.Error("expected error for invalid server nonce size")
	}
}

func TestSessionKeySymmetry(t *testing.T) {
	// Verify that client and server derive the same key
	sharedSecret := "test-shared-secret"
	clientNonce, _ := GenerateNonce()
	serverNonce, _ := GenerateNonce()

	// Simulate client side key derivation
	clientKey, err := DeriveSessionKey(sharedSecret, clientNonce, serverNonce)
	if err != nil {
		t.Fatalf("client DeriveSessionKey failed: %v", err)
	}

	// Simulate server side key derivation (same order: client, server)
	serverKey, err := DeriveSessionKey(sharedSecret, clientNonce, serverNonce)
	if err != nil {
		t.Fatalf("server DeriveSessionKey failed: %v", err)
	}

	if !bytes.Equal(clientKey, serverKey) {
		t.Error("client and server should derive the same session key")
	}

	// Create ciphers and verify they can decrypt each other's messages
	clientCipher, _ := NewXChaCha20Cipher(clientKey)
	serverCipher, _ := NewXChaCha20Cipher(serverKey)

	// Client encrypts, server decrypts
	plaintext := []byte("hello from client")
	ciphertext, err := clientCipher.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("client Encrypt failed: %v", err)
	}
	decrypted, err := serverCipher.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("server Decrypt failed: %v", err)
	}
	if !bytes.Equal(plaintext, decrypted) {
		t.Error("server should decrypt client's message")
	}

	// Server encrypts, client decrypts
	plaintext2 := []byte("hello from server")
	ciphertext2, err := serverCipher.Encrypt(plaintext2)
	if err != nil {
		t.Fatalf("server Encrypt failed: %v", err)
	}
	decrypted2, err := clientCipher.Decrypt(ciphertext2)
	if err != nil {
		t.Fatalf("client Decrypt failed: %v", err)
	}
	if !bytes.Equal(plaintext2, decrypted2) {
		t.Error("client should decrypt server's message")
	}
}

func BenchmarkEncrypt(b *testing.B) {
	key := make([]byte, KeySize)
	rand.Read(key)
	cipher, _ := NewXChaCha20Cipher(key)

	plaintext := make([]byte, 1024) // 1KB
	rand.Read(plaintext)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cipher.Encrypt(plaintext)
	}
}

func BenchmarkDecrypt(b *testing.B) {
	key := make([]byte, KeySize)
	rand.Read(key)
	cipher, _ := NewXChaCha20Cipher(key)

	plaintext := make([]byte, 1024) // 1KB
	rand.Read(plaintext)
	ciphertext, _ := cipher.Encrypt(plaintext)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cipher.Decrypt(ciphertext)
	}
}
