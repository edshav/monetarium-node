// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"math/big"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/chaingen"
	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/wire"
)

// newDisapprovedSpendHarness returns a harness whose tip is block "bn", which
// spends output x (returned) in its regular tree and outs[1:] (also returned)
// in its stake tree via ticket purchases, along with the name of the block the
// utxo cache was last flushed at.
//
// The chain never latches current, so, as during initial sync, connecting a
// block does not force a cache flush.  The cache is flushed once right before
// "bn" so x is in the backend and its cache entry is not fresh: a fresh entry
// spent in "bn" would be pruned to nil instead of being marked spent and would
// never reach the already-spent branch of UtxoCache.spendEntry.
//
//	... -> bsv111 -> bn
//	       ^^^^^^
//	     last flush
func newDisapprovedSpendHarness(t *testing.T) (*chaingenHarness, string, chaingen.SpendableOut, []chaingen.SpendableOut) {
	t.Helper()

	params := chaincfg.RegNetParams()
	params.MinKnownChainWork = new(big.Int).Lsh(big.NewInt(1), 255)
	g := newChaingenHarness(t, params)
	g.AdvanceToStakeValidationHeight()
	g.ExpectIsCurrent(false)

	// Force a cache flush at the tip; the chain keeps running.
	g.chain.ShutdownUtxoCache()
	flushed := g.TipName()

	outs := g.OldestCoinbaseOuts()
	g.NextBlock("bn", &outs[0], outs[1:])
	g.AcceptTipBlock()
	return g, flushed, outs[0], outs[1:]
}

// disapproveParent is a munger that makes the block vote against the regular
// transaction tree of its parent.
func disapproveParent(g *chaingenHarness) func(*wire.MsgBlock) {
	const (
		// voteBitNo and voteBitYes represent no and yes votes, respectively, on
		// whether or not to approve the previous block.
		voteBitNo  = 0x0000
		voteBitYes = 0x0001
	)

	return func(b *wire.MsgBlock) {
		b.Header.VoteBits &^= voteBitYes
		g.ReplaceVoteBits(voteBitNo)(b)
	}
}

// addSpend is a munger that adds a transaction spending the given output.
// Distinct fees give distinct transactions for the same output.
func addSpend(g *chaingenHarness, out chaingen.SpendableOut, fee dcrutil.Amount) func(*wire.MsgBlock) {
	return func(b *wire.MsgBlock) {
		b.AddTransaction(g.CreateSpendTx(&out, fee))
	}
}

// TestDisapprovedSpendRespentInCache ensures a block that disapproves its
// parent and spends again an output the parent's regular tree spent is accepted
// while the parent's spend is still unflushed in the utxo cache, and that the
// output stays spent afterwards.
func TestDisapprovedSpendRespentInCache(t *testing.T) {
	g, flushed, x, _ := newDisapprovedSpendHarness(t)

	// ---------------------------------------------------------------------
	// Create a block that disapproves bn, which undoes bn's spend of x, and
	// spends x again.  Committing it marks x spent in a cache that already
	// holds bn's unflushed spend of x.
	//
	//   ... -> bn -> bn1 (disapproves bn, spends x)
	// ---------------------------------------------------------------------

	outs := g.OldestCoinbaseOuts()
	g.NextBlock("bn1", nil, outs[1:], disapproveParent(g), addSpend(g, x, 1))
	g.AssertTipDisapprovesPrevious()
	g.ExpectUtxoSetState(flushed)
	g.AcceptTipBlock()

	// ---------------------------------------------------------------------
	// Create a block that spends x yet again.  It must be rejected since x is
	// spent by bn1.
	//
	//   ... -> bn -> bn1 -> bn2 (spends x)
	// ---------------------------------------------------------------------

	outs = g.OldestCoinbaseOuts()
	g.NextBlock("bn2", nil, outs[1:], addSpend(g, x, 3))
	g.ExpectUtxoSetState(flushed)
	g.RejectTipBlock(ErrMissingTxOut)
}

// TestDisapprovedSpendDoubleSpends ensures blocks that differ in one way from a
// block that disapproves its parent and spends again an output the parent's
// regular tree spent, each turning that into a real double spend, are rejected
// while the parent's spend is still unflushed in the utxo cache.
func TestDisapprovedSpendDoubleSpends(t *testing.T) {
	g, flushed, x, ticketOuts := newDisapprovedSpendHarness(t)
	outs := g.OldestCoinbaseOuts()

	// ---------------------------------------------------------------------
	// Create a block that disapproves bn and spends x twice.
	//
	//   ... -> bn
	//            \-> bn1dbl (disapproves bn, spends x twice)
	// ---------------------------------------------------------------------

	g.SetTip("bn")
	g.NextBlock("bn1dbl", nil, outs[1:], disapproveParent(g),
		addSpend(g, x, 1), addSpend(g, x, 3))
	g.AssertTipDisapprovesPrevious()
	g.ExpectUtxoSetState(flushed)
	g.RejectTipBlock(ErrMissingTxOut)

	// ---------------------------------------------------------------------
	// Create a block that spends x again without disapproving bn.
	//
	//   ... -> bn
	//            \-> bn1appr (approves bn, spends x)
	// ---------------------------------------------------------------------

	g.SetTip("bn")
	g.NextBlock("bn1appr", nil, outs[1:], addSpend(g, x, 1))
	g.RejectTipBlock(ErrMissingTxOut)

	// ---------------------------------------------------------------------
	// Create a block that disapproves bn and spends an output bn spent in its
	// stake tree, which disapproval does not undo.
	//
	//   ... -> bn
	//            \-> bn1stake (disapproves bn, spends ticket input of bn)
	// ---------------------------------------------------------------------

	g.SetTip("bn")
	g.NextBlock("bn1stake", nil, outs[1:], disapproveParent(g),
		addSpend(g, ticketOuts[0], 1))
	g.AssertTipDisapprovesPrevious()
	g.RejectTipBlock(ErrMissingTxOut)
}
