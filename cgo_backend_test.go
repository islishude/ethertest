package ethertest

import (
	"testing"

	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/secp256k1"
)

func TestSecp256k1UsesCGOBackend(t *testing.T) {
	if _, ok := gethcrypto.S256().(*secp256k1.BitCurve); !ok {
		t.Fatalf("secp256k1 backend = %T, want cgo BitCurve", gethcrypto.S256())
	}
}
