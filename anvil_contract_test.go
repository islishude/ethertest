package ethertest

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Verify actual registration candidates against the offline contract. This
// catches new ethertest methods accidentally leaking into compatibility APIs.
func TestAnvilContractRegistration(t *testing.T) {
	data, err := os.ReadFile("specs/upstream/anvil-compatibility.json")
	if err != nil {
		t.Fatal(err)
	}
	var matrix struct {
		Ref     string `json:"ref"`
		Commit  string `json:"commit"`
		Methods []struct {
			Method          string
			Aliases         []string
			Status          string
			RegisteredNames []string
			FirstRound      bool
			Tests           []string
		}
		DeprecatedAliases []string
	}
	if err := json.Unmarshal(data, &matrix); err != nil {
		t.Fatal(err)
	}
	if matrix.Ref != "v1.7.1" || matrix.Commit != "4072e48705af9d93e3c0f6e29e93b5e9a40caed8" || len(matrix.Methods) != 142 {
		t.Fatal("reference contract drift")
	}
	expected := make(map[string]bool)
	for _, row := range matrix.Methods {
		if row.FirstRound && len(row.Tests) == 0 {
			t.Errorf("%s has no evidence", row.Method)
		}
		for _, name := range row.RegisteredNames {
			if strings.HasPrefix(name, "anvil_") || strings.HasPrefix(name, "evm_") {
				expected[name] = true
			}
		}
	}
	for _, name := range matrix.DeprecatedAliases {
		expected[name] = true
	}
	actual := make(map[string]bool)
	for ns, typ := range map[string]reflect.Type{"anvil": reflect.TypeFor[*anvilAPI](), "evm": reflect.TypeFor[*evmAPI]()} {
		for method := range typ.Methods() {
			name := method.Name
			actual[ns+"_"+strings.ToLower(name[:1])+name[1:]] = true
		}
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("compatibility registration differs from matrix: actual=%v expected=%v", actual, expected)
	}
}
