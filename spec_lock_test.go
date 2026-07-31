package ethertest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestSpecLockDigests(t *testing.T) {
	var lock struct {
		Sources []struct {
			Name            string `toml:"name"`
			Subset          string `toml:"subset"`
			SubsetSHA256    string `toml:"subset_sha256"`
			Generated       string `toml:"generated"`
			GeneratedSHA256 string `toml:"generated_sha256"`
		} `toml:"source"`
	}
	if _, err := toml.DecodeFile("spec.lock", &lock); err != nil {
		t.Fatal(err)
	}
	for _, source := range lock.Sources {
		if source.Subset != "" {
			assertLockedDigest(t, source.Name+" subset", source.Subset, source.SubsetSHA256)
		}
		if source.Generated != "" {
			assertLockedDigest(t, source.Name+" generated output", source.Generated, source.GeneratedSHA256)
		}
	}
}

func assertLockedDigest(t *testing.T, name, path, want string) {
	t.Helper()
	if want == "" {
		t.Fatalf("%s %s has no locked digest", name, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if got := hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("%s digest = %s, want %s", name, got, want)
	}
}
