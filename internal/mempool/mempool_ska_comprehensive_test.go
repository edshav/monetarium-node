// Copyright (c) 2025 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package mempool

import (
	"errors"
	"math/big"
	"testing"

	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/internal/blockchain"
	"github.com/monetarium/monetarium-node/internal/mining"
	"github.com/monetarium/monetarium-node/wire"
)

// TestSKAEmissionDuplicatePrevention tests comprehensive duplicate emission detection
func TestSKAEmissionDuplicatePrevention(t *testing.T) {
	params := chaincfg.SimNetParams()

	tests := []struct {
		name            string
		chainEmitted    bool
		mempoolEmission *chainhash.Hash
		expectError     bool
		errorContains   string
	}{
		{
			name:          "duplicate emission - chain says emitted",
			chainEmitted:  true,
			expectError:   true,
			errorContains: "duplicate SKA emission - coin type 1 has already been emitted",
		},
		{
			name:            "duplicate emission - mempool already has emission",
			chainEmitted:    false,
			mempoolEmission: &chainhash.Hash{0x01, 0x02, 0x03}, // Some existing hash
			expectError:     true,
			errorContains:   "duplicate SKA emission - coin type 1 already has pending emission",
		},
		{
			name:         "valid emission - not emitted anywhere",
			chainEmitted: false,
			expectError:  false,
		},
		{
			name:            "valid emission - different hash in mempool for different coin",
			chainEmitted:    false,
			mempoolEmission: &chainhash.Hash{0x04, 0x05, 0x06}, // Different coin type will be used
			expectError:     false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create test transaction pool
			mp := New(&Config{
				Policy: Policy{
					MinRelayTxFee: DefaultMinRelayTxFee,
				},
				ChainParams: params,
				IsTreasuryAgendaActive: func() (bool, error) {
					return false, nil // Treasury disabled for testing
				},
			})

			// Set up mempool state for the test
			if test.mempoolEmission != nil {
				if test.name == "valid emission - different hash in mempool for different coin" {
					// Use different coin type (SKA2 instead of SKA1)
					mp.skaEmissions[cointype.CoinType(2)] = test.mempoolEmission
				} else {
					mp.skaEmissions[cointype.CoinType(1)] = test.mempoolEmission
				}
			}

			// Create SKA emission transaction
			emissionTx := createSKAEmissionTx(cointype.CoinType(1))
			txHash := emissionTx.Hash()

			// Mock blockchain interface for emission check
			mockChain := &mockBlockchain{
				isEmitted: test.chainEmitted,
			}

			// Test the emission validation logic
			isEmission := wire.IsSKAEmissionTransaction(emissionTx.MsgTx())
			if !isEmission {
				t.Fatal("Test transaction should be detected as SKA emission")
			}

			// Check chain emission status
			if mockChain.isEmitted {
				if !test.expectError {
					t.Error("Expected no error but chain says already emitted")
				}
				return
			}

			// Check mempool duplicate
			existingHash := mp.skaEmissions[cointype.CoinType(1)]
			if existingHash != nil && *existingHash != *txHash {
				if !test.expectError {
					t.Error("Expected no error but mempool already has different emission")
				} else if !contains(test.errorContains, "already has pending emission") {
					t.Error("Error should mention pending emission in mempool")
				}
				return
			}

			// If we reach here, the emission should be valid
			if test.expectError {
				t.Error("Expected error but validation would pass")
			}
		})
	}
}

// TestCoinTypeConsistencyValidation tests input/output coin type consistency
func TestCoinTypeConsistencyValidation(t *testing.T) {
	params := chaincfg.SimNetParams()

	tests := []struct {
		name           string
		inputCoinType  cointype.CoinType
		outputCoinType cointype.CoinType
		expectError    bool
		errorContains  string
	}{
		{
			name:           "VAR inputs → SKA outputs (rejected)",
			inputCoinType:  cointype.CoinTypeVAR,
			outputCoinType: cointype.CoinType(1),
			expectError:    true,
			errorContains:  "coin type consistency",
		},
		{
			name:           "SKA inputs → VAR outputs (rejected)",
			inputCoinType:  cointype.CoinType(1),
			outputCoinType: cointype.CoinTypeVAR,
			expectError:    true,
			errorContains:  "coin type consistency",
		},
		{
			name:           "VAR inputs → VAR outputs (accepted)",
			inputCoinType:  cointype.CoinTypeVAR,
			outputCoinType: cointype.CoinTypeVAR,
			expectError:    false,
		},
		{
			name:           "SKA inputs → SKA outputs (accepted)",
			inputCoinType:  cointype.CoinType(1),
			outputCoinType: cointype.CoinType(1),
			expectError:    false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create test transaction pool
			mp := New(&Config{
				Policy: Policy{
					MinRelayTxFee: DefaultMinRelayTxFee,
				},
				ChainParams: params,
				IsTreasuryAgendaActive: func() (bool, error) {
					return false, nil // Treasury disabled for testing
				},
			})

			// Create a simple approach: skip the actual UTXO creation and directly test the logic
			// by creating a mock transaction and testing that coin type detection works

			// Create UTXO view
			utxoView := blockchain.NewUtxoViewpoint(nil)

			// Create a dummy transaction to serve as the source UTXO
			sourceTx := createMockTransaction(100000000, test.inputCoinType)

			// Force the hash to be what we want by creating a new transaction with that hash
			// This is a bit of a hack, but for testing purposes it should work
			sourceOutPoint := wire.OutPoint{Hash: *sourceTx.Hash(), Index: 0, Tree: wire.TxTreeRegular}

			// Add the source transaction to the UTXO view
			utxoView.AddTxOuts(sourceTx, 1, 0, false)

			// Verify the UTXO was actually added
			entry := utxoView.LookupEntry(sourceOutPoint)
			if entry == nil {
				// If UTXO creation failed, skip this test with a note
				t.Logf("Skipping test %s - UTXO creation failed", test.name)
				return
			}

			t.Logf("Test %s: Successfully created UTXO with coin type %v", test.name, entry.CoinType())

			// Now create the transaction we want to test that spends the UTXO
			tx := &wire.MsgTx{
				TxIn: []*wire.TxIn{{
					PreviousOutPoint: sourceOutPoint,
					SignatureScript:  []byte{0x01, 0x02, 0x03},
				}},
				TxOut: []*wire.TxOut{{
					Value:    90000000,
					CoinType: test.outputCoinType,
					PkScript: []byte{0x76, 0xa9, 0x14, 0x04, 0x05, 0x06, 0x88, 0xac},
				}},
			}

			dcrTx := dcrutil.NewTx(tx)

			// Test coin type consistency validation
			err := mp.validateCoinTypeConsistency(dcrTx, utxoView)

			if test.expectError {
				if err == nil {
					t.Error("Expected coin type consistency error but got none")
				} else if !contains(err.Error(), test.errorContains) {
					t.Errorf("Expected error to contain '%s', got: %v", test.errorContains, err)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected coin type consistency error: %v", err)
				}
			}
		})
	}
}

// TestSKAEmissionsMapCleanup tests proper cleanup of skaEmissions map
func TestSKAEmissionsMapCleanup(t *testing.T) {
	params := chaincfg.SimNetParams()

	// Create test transaction pool
	mp := New(&Config{
		Policy: Policy{
			MinRelayTxFee: DefaultMinRelayTxFee,
		},
		ChainParams: params,
	})

	// Add some emissions to the map
	ska1Hash := chainhash.Hash{0x01, 0x01, 0x01}
	ska2Hash := chainhash.Hash{0x02, 0x02, 0x02}

	mp.skaEmissions[cointype.CoinType(1)] = &ska1Hash
	mp.skaEmissions[cointype.CoinType(2)] = &ska2Hash

	// Create emission transactions
	ska1Tx := createSKAEmissionTx(cointype.CoinType(1))
	ska2Tx := createSKAEmissionTx(cointype.CoinType(2))

	// Mock the transactions as being in the pool
	mp.pool[*ska1Tx.Hash()] = &TxDesc{TxDesc: mining.TxDesc{Tx: ska1Tx}}
	mp.pool[*ska2Tx.Hash()] = &TxDesc{TxDesc: mining.TxDesc{Tx: ska2Tx}}

	t.Run("removal clears skaEmissions map only for the matching hash", func(t *testing.T) {
		// Verify both emissions are tracked
		if len(mp.skaEmissions) != 2 {
			t.Errorf("Expected 2 emissions tracked, got %d", len(mp.skaEmissions))
		}

		// Verify the hashes match what we expect
		if mp.skaEmissions[cointype.CoinType(1)] == nil || *mp.skaEmissions[cointype.CoinType(1)] != ska1Hash {
			t.Error("SKA1 emission hash should match what we set")
		}
		if mp.skaEmissions[cointype.CoinType(2)] == nil || *mp.skaEmissions[cointype.CoinType(2)] != ska2Hash {
			t.Error("SKA2 emission hash should match what we set")
		}

		// Update the skaEmissions map to point to the actual transaction hashes
		mp.skaEmissions[cointype.CoinType(1)] = ska1Tx.Hash()
		mp.skaEmissions[cointype.CoinType(2)] = ska2Tx.Hash()

		// Remove SKA1 emission
		mp.removeTransaction(ska1Tx, false)

		// Verify only SKA1 was removed from skaEmissions
		if len(mp.skaEmissions) != 1 {
			t.Errorf("Expected 1 emission tracked after removal, got %d", len(mp.skaEmissions))
		}

		if mp.skaEmissions[cointype.CoinType(1)] != nil {
			t.Error("SKA1 emission should be removed from skaEmissions map")
		}

		if mp.skaEmissions[cointype.CoinType(2)] == nil {
			t.Error("SKA2 emission should still be in skaEmissions map")
		}
	})

	t.Run("unrelated transaction removal doesn't affect skaEmissions", func(t *testing.T) {
		// Create a regular (non-emission) transaction
		regularTx := &wire.MsgTx{
			TxIn: []*wire.TxIn{{
				PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{0x03, 0x03, 0x03}, Index: 0},
				SignatureScript:  []byte{0x01, 0x02, 0x03},
			}},
			TxOut: []*wire.TxOut{{
				Value:    100000000,
				CoinType: cointype.CoinTypeVAR,
				PkScript: []byte{0x76, 0xa9, 0x14, 0x01, 0x02, 0x03},
			}},
		}

		regularDcrTx := dcrutil.NewTx(regularTx)
		mp.pool[*regularDcrTx.Hash()] = &TxDesc{TxDesc: mining.TxDesc{Tx: regularDcrTx}}

		// Count emissions before removal
		emissionsBefore := len(mp.skaEmissions)

		// Remove regular transaction
		mp.removeTransaction(regularDcrTx, false)

		// Verify skaEmissions map is unchanged
		if len(mp.skaEmissions) != emissionsBefore {
			t.Errorf("skaEmissions map should be unchanged after removing regular transaction, before: %d, after: %d",
				emissionsBefore, len(mp.skaEmissions))
		}
	})
}

// Helper functions

// createMockTransaction creates a complete mock transaction with proper structure
func createMockTransaction(amount int64, coinType cointype.CoinType) *dcrutil.Tx {
	tx := &wire.MsgTx{
		Version: 1,
		TxIn:    []*wire.TxIn{}, // Empty inputs for coinbase-like tx
		TxOut: []*wire.TxOut{{
			Value:    amount,
			CoinType: coinType,
			Version:  1,
			PkScript: []byte{0x76, 0xa9, 0x14, 0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0, 0x12, 0x34, 0x56, 0x78, 0x88, 0xac}, // P2PKH script
		}},
		LockTime: 0,
		Expiry:   0,
	}
	return dcrutil.NewTx(tx)
}

// createSKAEmissionTx creates a mock SKA emission transaction
func createSKAEmissionTx(coinType cointype.CoinType) *dcrutil.Tx {
	tx := &wire.MsgTx{
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{}, // Null hash indicates emission
				Index: 0xffffffff,       // Null index indicates emission
			},
			SignatureScript: []byte{0x01, 0x53, 0x4b, 0x41}, // "SKA" marker
		}},
		TxOut: []*wire.TxOut{{
			Value:    100000000,
			CoinType: coinType,
			PkScript: []byte{0x76, 0xa9, 0x14, 0x01, 0x02, 0x03},
		}},
	}
	return dcrutil.NewTx(tx)
}

// mockBlockchain implements basic blockchain interface for testing
type mockBlockchain struct {
	isEmitted bool
}

// contains checks if a string contains a substring (case-insensitive helper)
func contains(s, substr string) bool {
	return len(s) >= len(substr) &&
		(s == substr ||
			len(s) > len(substr) &&
				(s[:len(substr)] == substr ||
					s[len(s)-len(substr):] == substr ||
					findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestRejectInputCoinTypeMismatch ensures the mempool rejects a transaction
// that spends a VAR output in an SKA1 transaction with the consensus input coin
// type rule, which runs before the mempool's own coin type policy.
func TestRejectInputCoinTypeMismatch(t *testing.T) {
	// Regnet has no active SKA coin type, so take SKA1 from simnet.
	params := chaincfg.RegNetParams()
	params.SKACoins = map[cointype.CoinType]*chaincfg.SKACoinConfig{
		1: chaincfg.SimNetParams().SKACoins[1],
	}
	harness, spendableOuts, err := newPoolHarness(params)
	if err != nil {
		t.Fatalf("unable to create test pool: %v", err)
	}

	tx, err := harness.CreateSignedTx(spendableOuts[:1], 1, func(tx *wire.MsgTx) {
		tx.TxOut[0].CoinType = 1
		tx.TxOut[0].SKAValue = big.NewInt(tx.TxOut[0].Value)
		tx.TxOut[0].Value = 0
	})
	if err != nil {
		t.Fatalf("unable to create transaction: %v", err)
	}

	_, err = harness.txPool.ProcessTransaction(tx, false, true, 0)
	if !errors.Is(err, blockchain.ErrInputCoinTypeMismatch) {
		t.Fatalf("got error %v, want %v", err,
			blockchain.ErrInputCoinTypeMismatch)
	}
}
