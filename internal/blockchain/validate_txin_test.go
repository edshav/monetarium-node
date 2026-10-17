// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/standalone"
	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/wire"
)

// txInTestScript is a P2PKH script used by the outputs in the input tests.
var txInTestScript = []byte{
	0x76, 0xa9, 0x14, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
	17, 18, 19, 20, 0x88, 0xac,
}

// txInTestOutput is an output in the view of an input test along with the
// value fields the spending input declares for it.
type txInTestOutput struct {
	coinType   cointype.CoinType
	amount     int64
	skaAmount  *big.Int
	valueIn    int64
	skaValueIn *big.Int
}

// checkTxInTest runs CheckTransactionInputs on testnet, where SKA1 and SKA2
// are active, for a transaction spending the given outputs and paying the
// given outputs.
func checkTxInTest(t *testing.T, spends []txInTestOutput, outs []*wire.TxOut,
	checkFraudProof bool) error {
	t.Helper()

	params := chaincfg.TestNet3Params()
	view := NewUtxoViewpoint(nil)
	tx := wire.NewMsgTx()
	for i, spend := range spends {
		prevOut := wire.OutPoint{Hash: chainhash.Hash{byte(i + 1)}}
		view.Entries()[prevOut] = &UtxoEntry{
			amount:      spend.amount,
			skaAmount:   spend.skaAmount,
			coinType:    spend.coinType,
			pkScript:    txInTestScript,
			blockHeight: 100,
		}
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: prevOut,
			Sequence:         wire.MaxTxInSequenceNum,
			ValueIn:          spend.valueIn,
			SKAValueIn:       spend.skaValueIn,
			BlockHeight:      100,
		})
	}
	tx.TxOut = outs

	_, err := CheckTransactionInputs(standalone.NewSubsidyCache(params),
		dcrutil.NewTx(tx), 200, view, checkFraudProof, params, nil, false, false,
		standalone.SSVMonetarium)
	return err
}

// TestTxInputValueFields ensures both value fields of an input must match the
// output it spends: the atom value of an SKA input and the SKA value of a VAR
// input must be zero.
func TestTxInputValueFields(t *testing.T) {
	ska := big.NewInt(5_000_000_000_000_000_000)
	varOut := []*wire.TxOut{{Value: 900, PkScript: txInTestScript}}
	skaOut := []*wire.TxOut{{SKAValue: big.NewInt(100), CoinType: 1,
		PkScript: txInTestScript}}

	tests := []struct {
		name           string
		spends         []txInTestOutput
		outs           []*wire.TxOut
		skipFraudProof bool
		want           error
	}{{
		name:   "VAR input with matching values",
		spends: []txInTestOutput{{amount: 1000, valueIn: 1000}},
		outs:   varOut,
	}, {
		// A block template is checked before its fraud proofs are filled in.
		name: "VAR input with wrong values without the fraud proof check",
		spends: []txInTestOutput{{amount: 1000, valueIn: 999,
			skaValueIn: big.NewInt(1)}},
		outs:           varOut,
		skipFraudProof: true,
	}, {
		name: "VAR input with a zero SKA value",
		spends: []txInTestOutput{{amount: 1000, valueIn: 1000,
			skaValueIn: new(big.Int)}},
		outs: varOut,
	}, {
		name: "VAR input with an SKA value",
		spends: []txInTestOutput{{amount: 1000, valueIn: 1000,
			skaValueIn: big.NewInt(1)}},
		outs: varOut,
		want: ErrFraudAmountIn,
	}, {
		name: "SKA input with matching values",
		spends: []txInTestOutput{{coinType: 1, skaAmount: ska,
			skaValueIn: ska}},
		outs: skaOut,
	}, {
		name: "SKA input with a different SKA value",
		spends: []txInTestOutput{{coinType: 1, skaAmount: ska,
			skaValueIn: new(big.Int).Sub(ska, big.NewInt(1))}},
		outs: skaOut,
		want: ErrFraudAmountIn,
	}, {
		name:   "SKA input without an SKA value",
		spends: []txInTestOutput{{coinType: 1, skaAmount: ska}},
		outs:   skaOut,
		want:   ErrFraudAmountIn,
	}, {
		name: "SKA input with an atom value",
		spends: []txInTestOutput{{coinType: 1, skaAmount: ska, valueIn: 1,
			skaValueIn: ska}},
		outs: skaOut,
		want: ErrFraudAmountIn,
	}, {
		name: "SKA input with an atom value in a VAR transaction",
		spends: []txInTestOutput{{amount: 1000, valueIn: 1000}, {coinType: 1,
			skaAmount: ska, valueIn: 1_000_000_000_000, skaValueIn: ska}},
		outs: varOut,
		want: ErrFraudAmountIn,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := checkTxInTest(t, test.spends, test.outs,
				!test.skipFraudProof)
			if !errors.Is(err, test.want) {
				t.Fatalf("got error %v, want %v", err, test.want)
			}
		})
	}
}

// TestTxInputCoinType ensures every input of a transaction must spend an
// output of the coin type of the transaction, also when the fraud proof is not
// checked, as for a block template.
func TestTxInputCoinType(t *testing.T) {
	ska1, ska2 := big.NewInt(110), big.NewInt(1_000_000)
	varOut := []*wire.TxOut{{Value: 900, PkScript: txInTestScript}}
	ska1Out := []*wire.TxOut{{SKAValue: big.NewInt(100), CoinType: 1,
		PkScript: txInTestScript}}

	tests := []struct {
		name   string
		spends []txInTestOutput
		outs   []*wire.TxOut
		want   error
	}{{
		name: "SKA input in a VAR transaction",
		spends: []txInTestOutput{{amount: 1000, valueIn: 1000}, {coinType: 1,
			skaAmount: ska1, skaValueIn: ska1}},
		outs: varOut,
		want: ErrInputCoinTypeMismatch,
	}, {
		name: "SKA2 input in an SKA1 transaction",
		spends: []txInTestOutput{{coinType: 1, skaAmount: ska1,
			skaValueIn: ska1}, {coinType: 2, skaAmount: ska2,
			skaValueIn: ska2}},
		outs: ska1Out,
		want: ErrInputCoinTypeMismatch,
	}, {
		name: "only an SKA2 input in an SKA1 transaction",
		spends: []txInTestOutput{{coinType: 2, skaAmount: ska2,
			skaValueIn: ska2}},
		outs: ska1Out,
		want: ErrInputCoinTypeMismatch,
	}, {
		name: "VAR input in an SKA1 transaction",
		spends: []txInTestOutput{{coinType: 1, skaAmount: ska1,
			skaValueIn: ska1}, {amount: 1000, valueIn: 1000}},
		outs: ska1Out,
		want: ErrInputCoinTypeMismatch,
	}, {
		name: "SKA1 inputs in an SKA1 transaction",
		spends: []txInTestOutput{{coinType: 1, skaAmount: ska1,
			skaValueIn: ska1}, {coinType: 1, skaAmount: ska2,
			skaValueIn: ska2}},
		outs: ska1Out,
	}}

	for _, test := range tests {
		for _, checkFraudProof := range []bool{true, false} {
			name := fmt.Sprintf("%s (fraud proof %v)", test.name,
				checkFraudProof)
			t.Run(name, func(t *testing.T) {
				err := checkTxInTest(t, test.spends, test.outs,
					checkFraudProof)
				if !errors.Is(err, test.want) {
					t.Fatalf("got error %v, want %v", err, test.want)
				}
			})
		}
	}
}

// TestTxInputRulesRejectBlock ensures a block is rejected when one of its
// transactions breaks either input rule, so the rules apply when blocks are
// connected and not only when CheckTransactionInputs is called directly.
func TestTxInputRulesRejectBlock(t *testing.T) {
	// Regnet has no active SKA coin type, so take SKA1 from simnet.
	params := chaincfg.RegNetParams()
	params.SKACoins = map[cointype.CoinType]*chaincfg.SKACoinConfig{
		1: chaincfg.SimNetParams().SKACoins[1],
	}
	g := newChaingenHarness(t, params)
	g.AdvanceToStakeValidationHeight()
	baseName := g.TipName()

	tests := []struct {
		name  string
		munge func(*wire.MsgTx)
		want  ErrorKind
	}{{
		name: "SKA value on a VAR input",
		munge: func(tx *wire.MsgTx) {
			tx.TxIn[0].SKAValueIn = big.NewInt(1)
		},
		want: ErrFraudAmountIn,
	}, {
		name: "VAR input in an SKA1 transaction",
		munge: func(tx *wire.MsgTx) {
			tx.TxOut[0].CoinType = 1
			tx.TxOut[0].SKAValue = big.NewInt(tx.TxOut[0].Value)
			tx.TxOut[0].Value = 0
		},
		want: ErrInputCoinTypeMismatch,
	}}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g.SetTip(baseName)
			outs := g.OldestCoinbaseOuts()
			g.NextBlock(fmt.Sprintf("bbadin%d", i), &outs[0], outs[1:],
				func(b *wire.MsgBlock) {
					test.munge(b.Transactions[1])
				})
			g.RejectTipBlock(test.want)
		})
	}
}
