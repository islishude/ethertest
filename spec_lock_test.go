package ethertest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

type specLock struct {
	Sources []specLockSource `toml:"source"`
}

type specLockSource struct {
	Name            string `toml:"name"`
	Subset          string `toml:"subset"`
	SubsetSHA256    string `toml:"subset_sha256"`
	Generated       string `toml:"generated"`
	GeneratedSHA256 string `toml:"generated_sha256"`
}

func TestSpecLockDigests(t *testing.T) {
	lock := readSpecLock(t)
	for _, source := range lock.Sources {
		if source.Subset != "" {
			assertLockedDigest(t, source.Name+" subset", source.Subset, source.SubsetSHA256)
		}
		if source.Generated != "" {
			assertLockedDigest(t, source.Name+" generated output", source.Generated, source.GeneratedSHA256)
		}
	}
}

func TestSpecLockDigestPathsForceLF(t *testing.T) {
	lock := readSpecLock(t)
	data, err := os.ReadFile(".gitattributes")
	if err != nil {
		t.Fatal(err)
	}
	lfPaths := make(map[string]struct{})
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		attributes := make(map[string]struct{}, len(fields)-1)
		for _, attribute := range fields[1:] {
			attributes[attribute] = struct{}{}
		}
		_, text := attributes["text"]
		_, lf := attributes["eol=lf"]
		if text && lf {
			lfPaths[strings.TrimPrefix(fields[0], "/")] = struct{}{}
		}
	}
	for _, source := range lock.Sources {
		for _, path := range []string{source.Subset, source.Generated} {
			if path == "" {
				continue
			}
			if _, ok := lfPaths[path]; !ok {
				t.Errorf("locked digest path %s must have an explicit `text eol=lf` entry in .gitattributes", path)
			}
		}
	}
}

func TestExecutionAPIBeta7SubsetClassification(t *testing.T) {
	data, err := os.ReadFile("specs/upstream/execution-rpc-subset.json")
	if err != nil {
		t.Fatal(err)
	}
	var subset struct {
		Ref                string `json:"ref"`
		Commit             string `json:"commit"`
		TotalMethods       int    `json:"totalMethods"`
		ImplementedMethods int    `json:"implementedMethods"`
		ExcludedMethods    int    `json:"excludedMethods"`
		Methods            []struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Unblock string `json:"unblock"`
		} `json:"methods"`
	}
	if err := json.Unmarshal(data, &subset); err != nil {
		t.Fatal(err)
	}
	if subset.Ref != "v1.0.0-beta.7" || subset.Commit != "5aebdfdd45cadeb723be4bd45b4611b71c8b1c85" {
		t.Fatalf("execution API source = %s@%s", subset.Ref, subset.Commit)
	}
	if subset.TotalMethods != 78 || subset.ImplementedMethods != 49 || subset.ExcludedMethods != 29 || len(subset.Methods) != 78 {
		t.Fatalf("execution API counts = total:%d implemented:%d excluded:%d list:%d", subset.TotalMethods, subset.ImplementedMethods, subset.ExcludedMethods, len(subset.Methods))
	}
	seen := make(map[string]struct{}, len(subset.Methods))
	expectedStatus := make(map[string]string, len(beta7ImplementedMethods)+len(beta7ExcludedMethods))
	for _, name := range beta7ImplementedMethods {
		expectedStatus[name] = "implemented"
	}
	for _, name := range beta7ExcludedMethods {
		if previous := expectedStatus[name]; previous != "" {
			t.Fatalf("method %s appears in both registration audit lists", name)
		}
		expectedStatus[name] = "excluded"
	}
	implemented, excluded := 0, 0
	for _, method := range subset.Methods {
		if _, exists := seen[method.Name]; exists {
			t.Fatalf("duplicate execution method %s", method.Name)
		}
		seen[method.Name] = struct{}{}
		if want := expectedStatus[method.Name]; want == "" || method.Status != want {
			t.Fatalf("method %s status = %q, registration audit wants %q", method.Name, method.Status, want)
		}
		switch method.Status {
		case "implemented":
			implemented++
		case "excluded":
			excluded++
			if method.Reason == "" || method.Unblock == "" {
				t.Fatalf("excluded method %s has no reason or unblock condition", method.Name)
			}
		default:
			t.Fatalf("method %s has unknown status %q", method.Name, method.Status)
		}
	}
	if implemented != 49 || excluded != 29 {
		t.Fatalf("classified methods = %d implemented, %d excluded", implemented, excluded)
	}
	if len(seen) != len(expectedStatus) {
		t.Fatalf("subset has %d unique methods, registration audit has %d", len(seen), len(expectedStatus))
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

func readSpecLock(t *testing.T) specLock {
	t.Helper()
	var lock specLock
	if _, err := toml.DecodeFile("spec.lock", &lock); err != nil {
		t.Fatal(err)
	}
	return lock
}
