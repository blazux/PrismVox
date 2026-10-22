package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPythonWireFormatAndAuthentication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	if err := os.WriteFile(path, key, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SECRET_KEY_PATH", path)
	value := "enc:v1:AAECAwQFBgcICQoLNHu4b62AtnLuYfT51I0dA_e_5lhOcUzonIgzhLDb4NL5MUVv"
	plain, err := Decrypt(value, "mcp:42")
	if err != nil || plain != "synthetic credential" {
		t.Fatalf("decode: %q %v", plain, err)
	}
	if _, err = Decrypt(value, "mcp:43"); err == nil {
		t.Fatal("field substitution accepted")
	}
	key[0] ^= 1
	os.WriteFile(path, key, 0600)
	if _, err = Decrypt(value, "mcp:42"); err == nil {
		t.Fatal("wrong key accepted")
	}
	os.Remove(path)
	if _, err = Decrypt(value, "mcp:42"); err == nil {
		t.Fatal("missing key accepted")
	}
	if plain, err = Decrypt("old credential", "mcp:42"); err != nil || plain != "old credential" {
		t.Fatal("legacy read compatibility lost")
	}
}
