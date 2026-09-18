package keys

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An unset environment variable is an empty passphrase. The manager refuses to
// write a keystore under it, to open one with it and to save one with it, and
// names the rule that refused. An external-key manager has no passphrase and
// is not touched.
func TestEmptyPassphraseIsRefusedAtEveryEntryPoint(t *testing.T) {
	kp, err := GenerateKeyForNetwork("ssq")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("LoadOrCreate writes nothing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "keys.enc")
		m := NewManager(path, "")
		if err := m.LoadOrCreate(); !errors.Is(err, ErrPassphraseEmpty) {
			t.Fatalf("LoadOrCreate: %v, want ErrPassphraseEmpty", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("a keystore was written under an empty passphrase")
		}
	})
	t.Run("Load on a keystore written under a passphrase", func(t *testing.T) {
		path, _ := v2Keystore(t)
		m := NewManager(path, "")
		if err := m.Load(); !errors.Is(err, ErrPassphraseEmpty) {
			t.Fatalf("Load: %v, want ErrPassphraseEmpty", err)
		}
		if m.KeyCount() != 0 {
			t.Fatal("manager holds keys after a refused Load")
		}
	})
	t.Run("Save writes nothing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "keys.enc")
		m := NewManager(path, "")
		if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
			t.Fatalf("ImportPrivateKey reads no passphrase and must not be refused: %v", err)
		}
		if err := m.Save(); !errors.Is(err, ErrPassphraseEmpty) {
			t.Fatalf("Save: %v, want ErrPassphraseEmpty", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("a keystore was written under an empty passphrase")
		}
	})
	t.Run("an external-key manager has no passphrase and is unaffected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "keys.enc")
		m := newManagerWithKey(t, path, testExternalKey(t, "no passphrase"))
		if err := m.LoadOrCreate(); err != nil {
			t.Fatal(err)
		}
	})
}

// encoding/json matches a member to a field case-insensitively, so a member
// spelled differently from the tag this build writes would decode into the same
// field while a case-sensitive reader saw something else. Such a member is
// refused by its spelling, whether or not the canonical one is present too,
// at every level of the file.
func TestMemberNamesAreTheOnesThisBuildWrites(t *testing.T) {
	decoy := `[{"pubkey":"AAAA","address":"ssq1decoy","index":0}]`
	for _, tc := range []struct {
		name, member string
		edit         func(t *testing.T, path string)
	}{
		{"the real list under PubKeys and a decoy under pubkeys", "PubKeys", func(t *testing.T, path string) {
			renameMember(t, path, `"pubkeys":`, `"PubKeys":`)
			duplicateMember(t, path, `"PubKeys":`, `"pubkeys": `+decoy+`, `)
		}},
		{"a lone PubKeys with no duplicate", "PubKeys", func(t *testing.T, path string) {
			renameMember(t, path, `"pubkeys":`, `"PubKeys":`)
		}},
		{"Version beside version", "Version", func(t *testing.T, path string) {
			duplicateMember(t, path, `"version":`, `"Version": 2, `)
		}},
		{"M inside kdfparams", "M", func(t *testing.T, path string) {
			duplicateMember(t, path, `"m":`, `"M": 8, `)
		}},
		{"Address inside a public key entry", "Address", func(t *testing.T, path string) {
			duplicateMember(t, path, `"address":`, `"Address": "ssq1decoy", `)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, m := v2Keystore(t)
			tc.edit(t, path)
			err := m.Load()
			if !errors.Is(err, ErrKeystoreHeader) {
				t.Fatalf("Load: %v, want ErrKeystoreHeader", err)
			}
			if !strings.Contains(err.Error(), `"`+tc.member+`"`) {
				t.Errorf("the error does not name the member %s: %v", tc.member, err)
			}
			if m.KeyCount() != 0 {
				t.Error("manager holds keys after a refused Load")
			}
		})
	}
}

// renameMember changes one member's spelling in the file as written. The
// member has to appear exactly once, or the case would be editing something
// other than what it names.
func renameMember(t *testing.T, path, from, to string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(data, []byte(from)); n != 1 {
		t.Fatalf("the keystore names %s %d times, want once", from, n)
	}
	if err := os.WriteFile(path, bytes.Replace(data, []byte(from), []byte(to), 1), 0o600); err != nil {
		t.Fatal(err)
	}
}
