package ethertest

import (
	"crypto/ecdsa"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/tyler-smith/go-bip32"
	"github.com/tyler-smith/go-bip39"
)

type Account struct {
	Address    common.Address
	PrivateKey *ecdsa.PrivateKey
	Path       string
}

func DeriveAccounts(mnemonic string, count int) ([]Account, error) {
	if !bip39.IsMnemonicValid(mnemonic) {
		return nil, fmt.Errorf("invalid BIP-39 mnemonic")
	}
	master, err := bip32.NewMasterKey(bip39.NewSeed(mnemonic, ""))
	if err != nil {
		return nil, err
	}
	key := master
	for _, index := range []uint32{
		bip32.FirstHardenedChild + 44,
		bip32.FirstHardenedChild + 60,
		bip32.FirstHardenedChild,
		0,
	} {
		key, err = key.NewChildKey(index)
		if err != nil {
			return nil, err
		}
	}
	out := make([]Account, count)
	for i := range count {
		child, err := key.NewChildKey(uint32(i))
		if err != nil {
			return nil, err
		}
		privateKey, err := crypto.ToECDSA(child.Key)
		if err != nil {
			return nil, err
		}
		out[i] = Account{
			Address:    crypto.PubkeyToAddress(privateKey.PublicKey),
			PrivateKey: privateKey,
			Path:       fmt.Sprintf("m/44'/60'/0'/0/%d", i),
		}
	}
	return out, nil
}
