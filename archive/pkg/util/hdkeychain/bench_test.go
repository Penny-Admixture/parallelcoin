package hdkeychain_test

import (
	"testing"
	
	"github.com/p9c/pod/pkg/util/hdkeychain"
)

// bip0032MasterPriv1 is the master private extended key from the first set of test vectors in BIP0032.
const bip0032MasterPriv1 = "xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbP" +
	"y6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHi"

// BenchmarkDeriveHardened benchmarks how long it takes to derive a hardened child from a master private extended key.
func BenchmarkDeriveHardened(b *testing.B) {
	b.StopTimer()
	masterKey, e := hdkeychain.NewKeyFromString(bip0032MasterPriv1)
	if e != nil {
		b.Errorf("Failed to decode master seed: %v", e)
	}
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		_, _ = masterKey.Child(hdkeychain.HardenedKeyStart)
	}
}

// BenchmarkDeriveNormal benchmarks how long it takes to derive a normal (non-hardened) child from a master private
// extended key.
func BenchmarkDeriveNormal(b *testing.B) {
	b.StopTimer()
	masterKey, e := hdkeychain.NewKeyFromString(bip0032MasterPriv1)
	if e != nil {
		b.Errorf("Failed to decode master seed: %v", e)
	}
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		_, _ = masterKey.Child(0)
	}
}

// BenchmarkPrivToPub benchmarks how long it takes to convert a private extended key to a public extended key.
func BenchmarkPrivToPub(b *testing.B) {
	b.StopTimer()
	masterKey, e := hdkeychain.NewKeyFromString(bip0032MasterPriv1)
	if e != nil {
		b.Errorf("Failed to decode master seed: %v", e)
	}
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		_, _ = masterKey.Neuter()
	}
}

// BenchmarkDeserialize benchmarks how long it takes to deserialize a private extended key.
func BenchmarkDeserialize(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_, _ = hdkeychain.NewKeyFromString(bip0032MasterPriv1)
		
	}
}

// BenchmarkSerialize benchmarks how long it takes to serialize a private extended key.
func BenchmarkSerialize(b *testing.B) {
	b.StopTimer()
	masterKey, e := hdkeychain.NewKeyFromString(bip0032MasterPriv1)
	if e != nil {
		b.Errorf("Failed to decode master seed: %v", e)
	}
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		_ = masterKey.String()
	}
}
