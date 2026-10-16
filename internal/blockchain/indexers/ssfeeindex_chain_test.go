// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package indexers

import (
	"context"
	"math/big"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/chaingen"
	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/wire"
)

// TestSSFeeIndexSkipsSSFeeWithoutPayment ensures the SSFee index connects and
// disconnects a block with an SSFee transaction that has no payment output,
// and catches up past it after a restart.
func TestSSFeeIndexSkipsSSFeeWithoutPayment(t *testing.T) {
	db := setupDB(t)
	chain, err := newTestChain()
	if err != nil {
		t.Fatal(err)
	}
	g, err := chaingen.MakeGenerator(chaincfg.SimNetParams())
	if err != nil {
		t.Fatal(err)
	}

	// Add a block with an SSFee transaction that only has its marker.
	ssFee := wire.NewMsgTx()
	ssFee.Version = 3
	ssFee.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex},
		Sequence:         wire.MaxTxInSequenceNum,
		BlockHeight:      wire.NullBlockHeight,
		BlockIndex:       wire.NullBlockIndex,
	})
	ssFee.AddTxOut(&wire.TxOut{CoinType: 1, SKAValue: new(big.Int),
		PkScript: stake.CreateMinerSSFeeMarker(2)})
	if !stake.IsSSFee(ssFee) {
		t.Fatal("test transaction is not an SSFee transaction")
	}
	addBlock(t, chain, &g, "bk1")
	bk2 := dcrutil.NewBlock(g.NextBlock("bk2", nil, nil, func(b *wire.MsgBlock) {
		b.AddSTransaction(ssFee)
	}))
	if err := chain.AddBlock(bk2); err != nil {
		t.Fatal(err)
	}

	// Ensure the index catches up to the block.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subber := NewIndexSubscriber(ctx)
	go subber.Run(ctx)
	idx, err := NewSSFeeIndex(subber, db, chain)
	if err != nil {
		t.Fatal(err)
	}
	if err := subber.CatchUp(ctx, db, chain); err != nil {
		t.Fatal(err)
	}
	assertSSFeeIndexTip(t, idx, bk2)

	// Replace the block with another one and restart the index, which
	// disconnects the block and catches up to the new tip.
	g.SetTip("bk1")
	if err := chain.RemoveBlock(bk2); err != nil {
		t.Fatal(err)
	}
	bk2a := addBlock(t, chain, &g, "bk2a")
	subber.mtx.Lock()
	err = idx.sub.stop()
	subber.mtx.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	idx, err = NewSSFeeIndex(subber, db, chain)
	if err != nil {
		t.Fatal(err)
	}
	if err := subber.CatchUp(ctx, db, chain); err != nil {
		t.Fatal(err)
	}
	assertSSFeeIndexTip(t, idx, bk2a)
}

// assertSSFeeIndexTip fails the test unless the tip of the SSFee index is the
// given block.
func assertSSFeeIndexTip(t *testing.T, idx *SSFeeIndex, blk *dcrutil.Block) {
	t.Helper()
	tipHeight, tipHash, err := idx.Tip()
	if err != nil {
		t.Fatal(err)
	}
	if tipHeight != blk.Height() || *tipHash != *blk.Hash() {
		t.Fatalf("index tip is %s (height %d), want %s (height %d)", tipHash,
			tipHeight, blk.Hash(), blk.Height())
	}
}
