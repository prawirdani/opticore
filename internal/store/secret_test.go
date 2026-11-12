package store

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	for _, plain := range []string{"", "hunter2", strings.Repeat("x", 4096)} {
		sealed, err := SealSecret(key, []byte(plain))
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		if strings.Contains(sealed, plain) && plain != "" {
			t.Fatalf("ciphertext contains plaintext %q", plain)
		}
		got, err := OpenSecret(key, sealed)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if string(got) != plain {
			t.Fatalf("Open = %q, want %q", got, plain)
		}
	}

	sealed, err := SealSecret(key, []byte("hunter2"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	sealed2, err := SealSecret(key, []byte("hunter2"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed == sealed2 {
		t.Fatal("two seals produced identical ciphertext; nonce not random")
	}
}

func TestOpenTamper(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	sealed, err := SealSecret(key, []byte("hunter2"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	tampered := base64.StdEncoding.EncodeToString(raw)
	if _, err := OpenSecret(key, tampered); err == nil {
		t.Fatal("Open accepted tampered ciphertext")
	}

	wrongKey := []byte("ffffffffffffffffffffffffffffffff")
	if _, err := OpenSecret(wrongKey, sealed); err == nil {
		t.Fatal("Open accepted the wrong key")
	}
}

func TestSealKeyLength(t *testing.T) {
	if _, err := SealSecret(make([]byte, 16), []byte("x")); err == nil {
		t.Fatal("Seal accepted a 16-byte key")
	}
	if _, err := OpenSecret(make([]byte, 16), "abc"); err == nil {
		t.Fatal("Open accepted a 16-byte key")
	}
}

func TestIsSecretPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"InternetGatewayDevice.ManagementServer.Password", true},
		{"InternetGatewayDevice.ManagementServer.ConnectionRequestPassword", true},
		{"InternetGatewayDevice.X.KeyPassphrase", true},
		{"InternetGatewayDevice.X.PreSharedKey", true},
		{"InternetGatewayDevice.X.WEPKey", true},
		{"InternetGatewayDevice.X.ParameterKey", false},
		{"InternetGatewayDevice.ManagementServer.ConnectionRequestURL", false},
		{"InternetGatewayDevice.X.Username", false},
		{"Password", true},
		{"ParameterKey", false},
	}
	for _, tc := range tests {
		if got := IsSecretPath(tc.path); got != tc.want {
			t.Errorf("IsSecretPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
