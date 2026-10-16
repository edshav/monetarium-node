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
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrec/secp256k1"
	"github.com/monetarium/monetarium-node/txscript"
	"github.com/monetarium/monetarium-node/wire"
)

// TestSSFeeDistributesSKAFees ensures a block with an SKA1 emission in the
// emission window and blocks whose SSFee transactions distribute the SKA1 fees
// of the block, unscaled by the number of votes, are accepted.
func TestSSFeeDistributesSKAFees(t *testing.T) {
	// Use the simnet SKA1 configuration on regnet, which the generator
	// supports, and emit SKA1 to the pay-to-script-hash OP_TRUE address of
	// the generator with an emission key owned by the test.
	params := chaincfg.RegNetParams()
	gen, err := chaingen.MakeGenerator(params)
	if err != nil {
		t.Fatalf("failed to create generator: %v", err)
	}
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := chaincfg.SimNetParams().SKACoins[1]
	params.SKACoins = map[cointype.CoinType]*chaincfg.SKACoinConfig{1: cfg}
	cfg.EmissionKey = priv.PubKey()
	cfg.EmissionAddresses = []string{gen.P2shOpTrueAddr().String()}
	g := newChaingenHarnessWithGen(t, &gen)
	g.AdvanceToStakeValidationHeight()
	g.AdvanceToHeight(uint32(cfg.EmissionHeight)-1, 1)

	//   ... -> bemit
	emitHeight := int64(cfg.EmissionHeight)
	emission := signedSKA1EmissionTx(t, params, priv, cfg.EmissionAddresses,
		cfg.EmissionAmounts, emitHeight)
	emission.TxIn[0].ValueIn = wire.NullValueIn
	emission.TxIn[0].BlockHeight = wire.NullBlockHeight
	emission.TxIn[0].BlockIndex = wire.NullBlockIndex
	var emitIndex uint32
	outs := g.OldestCoinbaseOuts()
	g.NextBlock("bemit", nil, outs[1:], func(b *wire.MsgBlock) {
		b.AddTransaction(emission)
		emitIndex = uint32(len(b.Transactions) - 1)
	})
	g.AcceptTipBlock()

	// The emission output matures after coinbase maturity since the
	// emission has an expiry.
	g.AdvanceToHeight(uint32(emitHeight)+uint32(params.CoinbaseMaturity), 1)

	// skaFeeBlock returns a munger that adds an SKA1 transaction spending
	// prevOut, worth amount and created at prevHeight and prevIndex, that
	// pays the given fee, along with the staker and miner SSFee transactions
	// that distribute it.
	payScript := emission.TxOut[0].PkScript
	minerScript, err := stake.ConsolidationAddrToPkScript(make([]byte, 20))
	if err != nil {
		t.Fatal(err)
	}
	opTrueSigScript, err := txscript.NewScriptBuilder().
		AddData([]byte{txscript.OP_TRUE}).Script()
	if err != nil {
		t.Fatal(err)
	}
	skaFeeBlock := func(prevOut wire.OutPoint, amount *big.Int, prevHeight,
		prevIndex uint32, fee *big.Int) func(*wire.MsgBlock) {

		return func(b *wire.MsgBlock) {
			spend := wire.NewMsgTx()
			spend.Version = 3
			spend.AddTxIn(&wire.TxIn{
				PreviousOutPoint: prevOut,
				Sequence:         wire.MaxTxInSequenceNum,
				SKAValueIn:       amount,
				BlockHeight:      prevHeight,
				BlockIndex:       prevIndex,
				SignatureScript:  opTrueSigScript,
			})
			spend.AddTxOut(&wire.TxOut{CoinType: 1, PkScript: payScript,
				SKAValue: new(big.Int).Sub(amount, fee)})
			b.AddTransaction(spend)

			fees := wire.NewFeesByType()
			fees.AddBig(1, fee)
			work, stakeShare, _, _ := standalone.GetSubsidyProportions(
				standalone.SSVMonetarium)
			minerFees, stakerFees := wire.CalcFeeSplitByCoinType(fees, work,
				stakeShare)

			// All votes share one consolidation address, so the staker
			// fees go to a single SSFee transaction.
			var consolidation []byte
			for _, stx := range b.STransactions {
				if stake.IsSSGen(stx) {
					consolidation, err = stake.ExtractSSFeeConsolidationAddr(stx)
					if err != nil {
						t.Fatal(err)
					}
					break
				}
			}
			stakerScript, err := stake.ConsolidationAddrToPkScript(consolidation)
			if err != nil {
				t.Fatal(err)
			}
			height := int64(b.Header.Height)
			b.AddSTransaction(skaSSFeeTx(stake.CreateStakerSSFeeMarker(height, 0),
				stakerScript, stakerFees.GetBig(1)))
			b.AddSTransaction(skaSSFeeTx(stake.CreateMinerSSFeeMarker(height),
				minerScript, minerFees.GetBig(1)))
		}
	}

	// Spend the emission output with all votes.
	//
	//   ... -> bemit -> ... -> bska5
	fee := big.NewInt(1_000_000_000_000_001)
	emitOut := wire.OutPoint{Hash: emission.TxHash(), Index: 0}
	emitAmount := cfg.EmissionAmounts[0]
	outs = g.OldestCoinbaseOuts()
	g.NextBlock("bska5", nil, outs[1:], skaFeeBlock(emitOut, emitAmount,
		uint32(emitHeight), emitIndex, fee))
	g.AcceptTipBlock()

	// Spend its change with fewer votes.
	//
	//   ... -> bska5 -> bska4
	tip := g.Tip()
	change := tip.Transactions[len(tip.Transactions)-1]
	changeOut := wire.OutPoint{Hash: change.TxHash(), Index: 0}
	g.NextBlock("bska4", nil, nil, g.ReplaceWithNVotes(4),
		skaFeeBlock(changeOut, change.TxOut[0].SKAValue, tip.Header.Height,
			uint32(len(tip.Transactions)-1), fee))
	g.AcceptTipBlock()
}

// skaSSFeeTx returns a null-input SKA1 SSFee transaction with the given marker
// that pays amount to payScript.
func skaSSFeeTx(marker, payScript []byte, amount *big.Int) *wire.MsgTx {
	tx := wire.NewMsgTx()
	tx.Version = 3
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex},
		Sequence:         wire.MaxTxInSequenceNum,
		BlockHeight:      wire.NullBlockHeight,
		BlockIndex:       wire.NullBlockIndex,
	})
	tx.AddTxOut(&wire.TxOut{CoinType: cointype.CoinType(1),
		SKAValue: new(big.Int), PkScript: marker})
	tx.AddTxOut(&wire.TxOut{CoinType: cointype.CoinType(1), SKAValue: amount,
		PkScript: payScript})
	return tx
}
