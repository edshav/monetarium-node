// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"math/big"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/chaingen"
	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/blockchain/standalone"
	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/txscript/stdscript"
	"github.com/monetarium/monetarium-node/wire"
)

// TestTxInputRulesAcceptRegular ensures single-coin transactions whose input
// values match the outputs they spend are accepted.
func TestTxInputRulesAcceptRegular(t *testing.T) {
	ska1, ska2 := big.NewInt(110), big.NewInt(1_000_000)
	ska1Out := &wire.TxOut{SKAValue: big.NewInt(100), CoinType: 1,
		PkScript: txInTestScript}

	tests := []struct {
		name   string
		spends []txInTestOutput
		outs   []*wire.TxOut
	}{{
		name: "VAR inputs",
		spends: []txInTestOutput{{amount: 1000, valueIn: 1000},
			{amount: 500, valueIn: 500}},
		outs: []*wire.TxOut{{Value: 1400, PkScript: txInTestScript}},
	}, {
		name: "SKA1 inputs",
		spends: []txInTestOutput{{coinType: 1, skaAmount: ska1,
			skaValueIn: ska1}, {coinType: 1, skaAmount: ska2,
			skaValueIn: ska2}},
		outs: []*wire.TxOut{ska1Out},
	}, {
		name: "SKA2 input",
		spends: []txInTestOutput{{coinType: 2, skaAmount: ska2,
			skaValueIn: ska2}},
		outs: []*wire.TxOut{{SKAValue: big.NewInt(900_000), CoinType: 2,
			PkScript: txInTestScript}},
	}, {
		name: "SKA1 input with a zero value VAR data output",
		spends: []txInTestOutput{{coinType: 1, skaAmount: ska1,
			skaValueIn: ska1}},
		outs: []*wire.TxOut{{PkScript: []byte{0x6a, 0x01, 0x01}}, ska1Out},
	}, {
		name: "SKA1 burn",
		spends: []txInTestOutput{{coinType: 1, skaAmount: ska1,
			skaValueIn: ska1}},
		outs: []*wire.TxOut{{SKAValue: big.NewInt(100), CoinType: 1,
			PkScript: stdscript.NewSKABurnScriptV0(1)}},
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := checkTxInTest(t, test.spends, test.outs, true); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestTxInputRulesSkipNullInputs ensures the input value and coin type rules
// do not apply to null inputs, which spend no output and may carry values.
func TestTxInputRulesSkipNullInputs(t *testing.T) {
	params, priv := mainnetSKA1TestParams(t)
	cfg := params.SKACoins[1]
	height := int64(cfg.EmissionHeight)

	// An SKA1 emission with the null fraud proof it carries on mainnet.
	emission := signedSKA1EmissionTx(t, params, priv, cfg.EmissionAddresses,
		cfg.EmissionAmounts, height)
	emission.TxIn[0].ValueIn = wire.NullValueIn
	emission.TxIn[0].BlockHeight = wire.NullBlockHeight
	emission.TxIn[0].BlockIndex = wire.NullBlockIndex

	// A null-input SKA1 staker SSFee whose input carries the fee as its SKA
	// value.
	fee := big.NewInt(5_000)
	payScript, err := stake.ConsolidationAddrToPkScript(txInTestScript[3:23])
	if err != nil {
		t.Fatal(err)
	}
	ssFee := wire.NewMsgTx()
	ssFee.Version = 3
	ssFee.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{},
			Index: wire.MaxPrevOutIndex},
		Sequence:    wire.MaxTxInSequenceNum,
		BlockHeight: wire.NullBlockHeight,
		BlockIndex:  wire.NullBlockIndex,
		SKAValueIn:  fee,
	})
	ssFee.AddTxOut(&wire.TxOut{CoinType: 1, SKAValue: new(big.Int),
		PkScript: stake.CreateStakerSSFeeMarker(height, 0)})
	ssFee.AddTxOut(&wire.TxOut{CoinType: 1, SKAValue: fee,
		PkScript: payScript})

	for name, tx := range map[string]*wire.MsgTx{
		"SKA1 emission":         emission,
		"null-input SKA1 SSFee": ssFee,
	} {
		_, err := CheckTransactionInputs(standalone.NewSubsidyCache(params),
			dcrutil.NewTx(tx), height, NewUtxoViewpoint(nil), true, params,
			nil, false, false, standalone.SSVMonetarium)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
	}
}

// TestTxInputRulesAcceptStakeTxns ensures blocks with ticket purchases, votes,
// coinbases, null-input SSFee transactions, an augmented SSFee transaction
// whose input values match the SSFee output it spends and a revocation are
// accepted.
func TestTxInputRulesAcceptStakeTxns(t *testing.T) {
	g := newChaingenHarness(t, chaincfg.RegNetParams())
	g.AdvanceToStakeValidationHeight()

	// Add a block whose transactions pay fees so it has an SSFee.
	//
	//   ... -> bsvh -> bssf0
	outs := g.OldestCoinbaseOuts()
	g.NextBlock("bssf0", &outs[0], outs[1:])
	g.AcceptTipBlock()

	// Find the SSFee payout of the tip block.
	tip := g.Tip()
	var prevPayout *chaingen.SpendableOut
	for i, stx := range tip.STransactions {
		if stake.IsSSFee(stx) {
			out := chaingen.MakeSpendableStakeOut(tip, uint32(i), 0)
			prevPayout = &out
			break
		}
	}
	if prevPayout == nil {
		t.Fatal("tip block has no SSFee transaction")
	}

	// Make the SSFee of the next block add its fee to that payout.
	//
	//   ... -> bsvh -> bssf0 -> bssf1
	outs = g.OldestCoinbaseOuts()
	g.NextBlock("bssf1", &outs[0], outs[1:], func(b *wire.MsgBlock) {
		for i, stx := range b.STransactions {
			if !stake.IsSSFee(stx) {
				continue
			}
			amount := int64(prevPayout.Amount())
			augmented := stx.Copy()
			augmented.TxIn[0] = &wire.TxIn{
				PreviousOutPoint: prevPayout.PrevOut(),
				Sequence:         wire.MaxTxInSequenceNum,
				ValueIn:          amount,
				BlockHeight:      prevPayout.BlockHeight(),
				BlockIndex:       prevPayout.BlockIndex(),
			}
			augmented.TxOut[0].Value += amount
			b.STransactions[i] = augmented
			return
		}
		t.Fatal("block template has no SSFee transaction")
	})
	g.AcceptTipBlock()

	// Miss one of the winning tickets, which the generator revokes in the
	// next block.
	//
	//   ... -> bssf1 -> bmiss -> brevoke
	outs = g.OldestCoinbaseOuts()
	g.NextBlock("bmiss", &outs[0], nil, g.ReplaceWithNVotes(4))
	g.AcceptTipBlock()
	outs = g.OldestCoinbaseOuts()
	g.NextBlock("brevoke", &outs[0], nil)
	g.AssertTipNumRevocations(1)
	g.AcceptTipBlock()
}
