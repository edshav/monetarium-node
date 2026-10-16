// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/blockchain/standalone"
	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrec/secp256k1"
	"github.com/monetarium/monetarium-node/txscript"
	"github.com/monetarium/monetarium-node/txscript/stdaddr"
	"github.com/monetarium/monetarium-node/wire"
)

// skaDisconnectHarness bundles a chaingen harness that has emitted SKA1 to a
// p2sh OP_TRUE output, so the test can spend it without tracking keys.
type skaDisconnectHarness struct {
	*chaingenHarness
	emitAmount *big.Int
	emitOut    wire.OutPoint // the emitted SKA output
}

// newSKADisconnectHarness returns a regnet harness advanced past stake
// validation height whose block "bemit" holds an SKA1 emission paying the
// generator's p2sh OP_TRUE address, with the tip at the height where that
// output is mature.
func newSKADisconnectHarness(t *testing.T) *skaDisconnectHarness {
	t.Helper()

	params := cloneParams(chaincfg.RegNetParams())
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("failed to generate emission key: %v", err)
	}
	opTrueAddr, err := stdaddr.NewAddressScriptHashV0([]byte{txscript.OP_TRUE},
		params)
	if err != nil {
		t.Fatalf("failed to create p2sh OP_TRUE address: %v", err)
	}
	emitAmount := new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))
	emitHeight := params.StakeValidationHeight + 1
	params.SKACoins = map[cointype.CoinType]*chaincfg.SKACoinConfig{
		1: {
			CoinType:          1,
			Name:              "Skarb-1",
			Symbol:            "SKA1",
			MaxSupply:         emitAmount,
			AtomsPerCoin:      big.NewInt(1e18),
			EmissionHeight:    int32(emitHeight),
			EmissionWindow:    100,
			Active:            true,
			EmissionAddresses: []string{opTrueAddr.String()},
			EmissionAmounts:   []*big.Int{emitAmount},
			EmissionKey:       priv.PubKey(),
		},
	}
	params.InitialSKATypes = []cointype.CoinType{1}

	g := newChaingenHarness(t, params)
	g.AdvanceToStakeValidationHeight()

	emitTx := signedSKA1EmissionTx(t, params, priv,
		params.SKACoins[1].EmissionAddresses, []*big.Int{emitAmount},
		emitHeight)
	emitTx.TxIn[0].BlockHeight = wire.NullBlockHeight
	emitTx.TxIn[0].BlockIndex = wire.NullBlockIndex
	outs := g.OldestCoinbaseOuts()
	g.NextBlock("bemit", nil, outs[1:], func(b *wire.MsgBlock) {
		b.AddTransaction(emitTx)
	})
	g.SaveTipCoinbaseOuts()
	g.AcceptTipBlock()

	// Outputs of transactions with an expiry, such as the emission, require
	// coinbase maturity before they can be spent.
	for i := uint16(0); i < params.CoinbaseMaturity; i++ {
		outs := g.OldestCoinbaseOuts()
		g.NextBlock(fmt.Sprintf("bskamat%d", i), nil, outs[1:])
		g.SaveTipCoinbaseOuts()
		g.AcceptTipBlock()
	}

	return &skaDisconnectHarness{
		chaingenHarness: g,
		emitAmount:      emitAmount,
		emitOut: wire.OutPoint{
			Hash:  emitTx.TxHash(),
			Index: 0,
			Tree:  wire.TxTreeRegular,
		},
	}
}

// skaSpendFee is the SKA fee, in atoms, paid by the spend transaction.
const skaSpendFee = 10

// createSKASpendTx returns a transaction that spends the emitted SKA output in
// to a new p2sh OP_TRUE SKA output, paying skaSpendFee.
func (h *skaDisconnectHarness) createSKASpendTx() *wire.MsgTx {
	emitBlock := h.BlockByName("bemit")
	scriptVer, script := h.P2shOpTrueAddr().PaymentScript()
	tx := wire.NewMsgTx()
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: h.emitOut,
		Sequence:         wire.MaxTxInSequenceNum,
		SKAValueIn:       new(big.Int).Set(h.emitAmount),
		BlockHeight:      emitBlock.Header.Height,
		BlockIndex:       uint32(len(emitBlock.Transactions) - 1),
		SignatureScript:  []byte{txscript.OP_DATA_1, txscript.OP_TRUE},
	})
	tx.AddTxOut(&wire.TxOut{
		SKAValue: new(big.Int).Sub(h.emitAmount, big.NewInt(skaSpendFee)),
		CoinType: 1,
		Version:  scriptVer,
		PkScript: script,
	})
	return tx
}

// addSKASpendTx adds spendTx to the regular tree of b along with the stake
// tree SSFee transactions that distribute its SKA fee to the block's voters
// and miner.
func addSKASpendTx(b *wire.MsgBlock, spendTx *wire.MsgTx) {
	b.AddTransaction(spendTx)

	height := int64(b.Header.Height)
	minerFee, stakerFee := standalone.CalcFeeSplit(skaSpendFee,
		standalone.SSVMonetarium)

	// newSSFeeTx returns a null-input SSFee transaction with the given marker
	// paying amount to pkScript.
	newSSFeeTx := func(marker, pkScript []byte, amount int64) *wire.MsgTx {
		tx := wire.NewMsgTx()
		tx.Version = 3
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *wire.NewOutPoint(&chainhash.Hash{},
				wire.MaxPrevOutIndex, wire.TxTreeRegular),
			Sequence:    wire.MaxTxInSequenceNum,
			BlockHeight: wire.NullBlockHeight,
			BlockIndex:  wire.NullBlockIndex,
		})
		tx.AddTxOut(&wire.TxOut{
			SKAValue: new(big.Int),
			CoinType: 1,
			PkScript: marker,
		})
		tx.AddTxOut(&wire.TxOut{
			SKAValue: big.NewInt(amount),
			CoinType: 1,
			PkScript: pkScript,
		})
		return tx
	}

	// Staker share.  Every chaingen vote names the same consolidation address,
	// so the whole share goes to it in a single SSFee keyed by the first vote.
	hash160, err := stake.ExtractSSFeeConsolidationAddr(b.STransactions[0])
	if err != nil {
		panic(err)
	}
	pkScript, err := stake.ConsolidationAddrToPkScript(hash160)
	if err != nil {
		panic(err)
	}
	b.AddSTransaction(newSSFeeTx(stake.CreateStakerSSFeeMarker(height, 0),
		pkScript, stakerFee))

	// Miner share to an OP_SSGEN-tagged anyone-can-spend script.
	b.AddSTransaction(newSSFeeTx(stake.CreateMinerSSFeeMarker(height),
		[]byte{txscript.OP_SSGEN, txscript.OP_TRUE}, minerFee))
}

// assertSKASpendUndone ensures the utxo set at the current tip no longer
// holds the output created by spendTx and holds the emitted output it spent
// as unspent with its original SKA amount and coin type.
func (h *skaDisconnectHarness) assertSKASpendUndone(spendTx *wire.MsgTx) {
	t := h.t
	t.Helper()

	created := wire.OutPoint{Hash: spendTx.TxHash(), Tree: wire.TxTreeRegular}
	entry, err := h.chain.FetchUtxoEntry(created)
	if err != nil {
		t.Fatalf("FetchUtxoEntry(%v): %v", created, err)
	}
	if entry != nil && !entry.IsSpent() {
		t.Errorf("SKA output %v created by the undone transaction is still "+
			"unspent in the utxo set (coin type %d, SKA amount %v)", created,
			entry.CoinType(), entry.SKAAmount())
	}

	entry, err = h.chain.FetchUtxoEntry(h.emitOut)
	if err != nil {
		t.Fatalf("FetchUtxoEntry(%v): %v", h.emitOut, err)
	}
	if entry == nil || entry.IsSpent() {
		t.Fatalf("SKA output %v spent by the undone transaction was not "+
			"restored to the utxo set", h.emitOut)
	}
	if got := entry.CoinType(); got != 1 {
		t.Fatalf("restored output %v coin type: got %d, want 1", h.emitOut, got)
	}
	if got := entry.SKAAmount(); got == nil || got.Cmp(h.emitAmount) != 0 {
		t.Fatalf("restored output %v SKA amount: got %v, want %v", h.emitOut,
			got, h.emitAmount)
	}
}

// assertSSFeeOutputsGone ensures the utxo set at the current tip holds none of
// the SKA outputs created by the null-input SSFee transactions in the stake
// tree of the disconnected block b.
func (h *skaDisconnectHarness) assertSSFeeOutputsGone(b *wire.MsgBlock) {
	t := h.t
	t.Helper()

	var checked int
	for _, stx := range b.STransactions {
		if stake.DetermineTxType(stx) != stake.TxTypeSSFee {
			continue
		}
		for i, txOut := range stx.TxOut {
			if !txOut.CoinType.IsSKA() || txOut.SKAValue == nil ||
				txOut.SKAValue.Sign() == 0 {
				continue
			}
			op := wire.OutPoint{Hash: stx.TxHash(), Index: uint32(i),
				Tree: wire.TxTreeStake}
			entry, err := h.chain.FetchUtxoEntry(op)
			if err != nil {
				t.Fatalf("FetchUtxoEntry(%v): %v", op, err)
			}
			if entry != nil && !entry.IsSpent() {
				t.Errorf("SKA SSFee output %v of the disconnected block is "+
					"still unspent in the utxo set (SKA amount %v)", op,
					entry.SKAAmount())
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("disconnected block has no SKA SSFee outputs to check")
	}
}

// flushUtxoCache forces the utxo cache to the backend so the following
// lookups read the persisted state.
func (h *skaDisconnectHarness) flushUtxoCache() {
	h.t.Helper()
	tip := h.chain.bestChain.Tip()
	err := h.chain.utxoCache.MaybeFlush(&tip.hash, uint32(tip.height), true,
		false)
	if err != nil {
		h.t.Fatalf("failed to flush utxo cache: %v", err)
	}
}

// TestDisconnectSKAOutputs ensures that undoing the regular transactions of a
// block removes the SKA outputs those transactions created and restores the
// SKA outputs they spent, both when the next block disapproves the regular
// tree and when the block is disconnected by a reorganization.
func TestDisconnectSKAOutputs(t *testing.T) {
	const (
		// vbDisapprovePrev and vbApprovePrev represent no and yes votes,
		// respectively, on whether or not to approve the previous block.
		vbDisapprovePrev = 0x0000
		vbApprovePrev    = 0x0001
	)

	t.Run("Disapproval", func(t *testing.T) {
		h := newSKADisconnectHarness(t)

		//   ... -> bskamat# -> bx (spends the SKA emission)
		spendTx := h.createSKASpendTx()
		outs := h.OldestCoinbaseOuts()
		h.NextBlock("bx", nil, outs[1:], func(b *wire.MsgBlock) {
			addSKASpendTx(b, spendTx)
		})
		h.SaveTipCoinbaseOuts()
		h.AcceptTipBlock()

		//   ... -> bskamat# -> bx -> by (disapproves bx)
		outs = h.OldestCoinbaseOuts()
		h.NextBlock("by", nil, outs[1:], func(b *wire.MsgBlock) {
			b.Header.VoteBits &^= vbApprovePrev
			h.ReplaceVoteBits(vbDisapprovePrev)(b)
		})
		h.AssertTipDisapprovesPrevious()
		h.SaveTipCoinbaseOuts()
		h.AcceptTipBlock()

		h.assertSKASpendUndone(spendTx)
		if t.Failed() {
			return
		}
		h.flushUtxoCache()
		h.assertSKASpendUndone(spendTx)
	})

	t.Run("Reorg", func(t *testing.T) {
		h := newSKADisconnectHarness(t)
		forkName := h.TipName()

		//   ... -> bskamat# -> bx (spends the SKA emission) -> by
		spendTx := h.createSKASpendTx()
		outs := h.OldestCoinbaseOuts()
		bx := h.NextBlock("bx", nil, outs[1:], func(b *wire.MsgBlock) {
			addSKASpendTx(b, spendTx)
		})
		h.AcceptTipBlock()
		h.NextBlock("by", nil, nil)
		h.AcceptTipBlock()

		//   ... -> bskamat# -> bx -> by
		//             \-> bx2 -> by2 -> bz2
		h.SetTip(forkName)
		h.NextBlock("bx2", nil, nil)
		h.AcceptedToSideChainWithExpectedTip("by")
		h.NextBlock("by2", nil, nil)
		h.AcceptedToSideChainWithExpectedTip("by")
		h.NextBlock("bz2", nil, nil)
		h.AcceptTipBlock()

		h.assertSKASpendUndone(spendTx)
		h.assertSSFeeOutputsGone(bx)
		if t.Failed() {
			return
		}
		h.flushUtxoCache()
		h.assertSKASpendUndone(spendTx)
		h.assertSSFeeOutputsGone(bx)
	})
}
