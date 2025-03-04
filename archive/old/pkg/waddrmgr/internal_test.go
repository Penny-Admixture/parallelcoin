/*
This test file is part of the waddrmgr package rather than than the
waddrmgr_test package so it can bridge access to the internals to properly test
cases which are either not possible or can't reliably be tested via the public
interface. The functions are only exported while the tests are being run.
*/
package waddrmgr

import (
	"errors"

	"github.com/p9c/pod/pkg/snacl"
)

// // TstLatestMgrVersion makes the unexported latestMgrVersion variable
// available // for change when the tests are run. var TstLatestMgrVersion =
// &latestMgrVersion

// TstCheckPublicPassphrase returns true if the provided public passphrase is correct for the manager.
func (m *Manager) TstCheckPublicPassphrase(pubPassphrase []byte) bool {
	secretKey := snacl.SecretKey{Key: &snacl.CryptoKey{}}
	secretKey.Parameters = m.masterKeyPub.Parameters
	e := secretKey.DeriveKey(&pubPassphrase)
	return e ==  nil
}

// failingCryptoKey is an implementation of the EncryptorDecryptor interface
// with intentionally fails when attempting to encrypt or decrypt with it.
type failingCryptoKey struct {
	cryptoKey
}

// Encrypt intenionally returns a failure when invoked to test error paths.
//
// This is part of the EncryptorDecryptor interface implementation.
func (c *failingCryptoKey) Encrypt(in []byte) ([]byte, error) {
	return nil, errors.New("failed to encrypt")
}

// Decrypt intenionally returns a failure when invoked to test error paths.
//
// This is part of the EncryptorDecryptor interface implementation.
func (c *failingCryptoKey) Decrypt(in []byte) ([]byte, error) {
	return nil, errors.New("failed to decrypt")
}

// TstRunWithFailingCryptoKeyPriv runs the provided callback with the private
// crypto key replaced with a version that fails to help test error paths.
func TstRunWithFailingCryptoKeyPriv(m *Manager, callback func()) {
	orig := m.cryptoKeyPriv
	defer func() {
		m.cryptoKeyPriv = orig
	}()
	m.cryptoKeyPriv = &failingCryptoKey{}
	callback()
}

// TstDefaultAccountName is the constant defaultAccountName exported for tests.
const TstDefaultAccountName = defaultAccountName
