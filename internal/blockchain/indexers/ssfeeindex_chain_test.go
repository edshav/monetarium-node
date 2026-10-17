// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package indexers

import (
	"bytes"
	"context"
	"encoding/hex"
	"math/big"
	"reflect"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/chaingen"
	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/database"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/wire"
)

var (
	// nullSSFeeInput is the previous outpoint of an SSFee transaction that
	// creates a new output instead of augmenting an existing one.
	nullSSFeeInput = wire.OutPoint{Index: wire.MaxPrevOutIndex}

	// testSSFeePayTo is the address hash160 test SSFee transactions pay to.
	testSSFeePayTo = bytes.Repeat([]byte{0x01}, 20)
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
	addBlock(t, chain, &g, "bk1")
	bk2 := addSSFeeBlock(t, chain, &g, "bk2",
		newTestSSFee(t, 2, nullSSFeeInput, nil))

	// Ensure the index catches up to the block.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subber := NewIndexSubscriber(ctx)
	go subber.Run(ctx)
	idx := startSSFeeIndex(ctx, t, subber, db, chain)
	assertSSFeeIndexTip(t, idx, bk2)

	// Replace the block with another one and restart the index, which
	// disconnects the block and catches up to the new tip.
	g.SetTip("bk1")
	if err := chain.RemoveBlock(bk2); err != nil {
		t.Fatal(err)
	}
	bk2a := addBlock(t, chain, &g, "bk2a")
	stopSSFeeIndex(t, subber, idx)
	idx = startSSFeeIndex(ctx, t, subber, db, chain)
	assertSSFeeIndexTip(t, idx, bk2a)
}

// TestSSFeeIndexIndexesSSFeeWithPayment ensures the SSFee index indexes the
// payment output of an SSFee transaction that has its marker and a payment.
func TestSSFeeIndexIndexesSSFeeWithPayment(t *testing.T) {
	db := setupDB(t)
	chain, err := newTestChain()
	if err != nil {
		t.Fatal(err)
	}
	g, err := chaingen.MakeGenerator(chaincfg.SimNetParams())
	if err != nil {
		t.Fatal(err)
	}

	addBlock(t, chain, &g, "bk1")
	ssFee := newTestSSFee(t, 2, nullSSFeeInput, testSSFeePayTo)
	addSSFeeBlock(t, chain, &g, "bk2", ssFee)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subber := NewIndexSubscriber(ctx)
	go subber.Run(ctx)
	startSSFeeIndex(ctx, t, subber, db, chain)
	assertSSFeeIndexEntries(t, db, map[string][]wire.OutPoint{
		ssfeeIndexEntryKey(t, ssFee): {ssFeePayment(ssFee)},
	})
}

// TestSSFeeIndexSkipsOnlySSFeeWithoutPayment ensures that skipping an SSFee
// transaction without a payment output does not skip the SSFee transaction
// after it in the same block.
func TestSSFeeIndexSkipsOnlySSFeeWithoutPayment(t *testing.T) {
	db := setupDB(t)
	chain, err := newTestChain()
	if err != nil {
		t.Fatal(err)
	}
	g, err := chaingen.MakeGenerator(chaincfg.SimNetParams())
	if err != nil {
		t.Fatal(err)
	}

	// The SSFee without a payment output comes first.
	addBlock(t, chain, &g, "bk1")
	ssFee := newTestSSFee(t, 2, nullSSFeeInput, testSSFeePayTo)
	addSSFeeBlock(t, chain, &g, "bk2",
		newTestSSFee(t, 2, nullSSFeeInput, nil), ssFee)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subber := NewIndexSubscriber(ctx)
	go subber.Run(ctx)
	startSSFeeIndex(ctx, t, subber, db, chain)
	assertSSFeeIndexEntries(t, db, map[string][]wire.OutPoint{
		ssfeeIndexEntryKey(t, ssFee): {ssFeePayment(ssFee)},
	})
}

// TestSSFeeIndexDisconnectRestoresEntries ensures that disconnecting a block
// with an SSFee transaction without a payment output followed by one with a
// payment restores exactly the entries the index held before the block was
// connected.
func TestSSFeeIndexDisconnectRestoresEntries(t *testing.T) {
	db := setupDB(t)
	chain, err := newTestChain()
	if err != nil {
		t.Fatal(err)
	}
	g, err := chaingen.MakeGenerator(chaincfg.SimNetParams())
	if err != nil {
		t.Fatal(err)
	}

	// Create the following chain, where bk3 pays to the same key as bk2:
	//
	//   genesis -> bk1 -> bk2 -> bk3
	//                        \-> bk3a
	addBlock(t, chain, &g, "bk1")
	ssFee2 := newTestSSFee(t, 2, nullSSFeeInput, testSSFeePayTo)
	addSSFeeBlock(t, chain, &g, "bk2", ssFee2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subber := NewIndexSubscriber(ctx)
	go subber.Run(ctx)
	idx := startSSFeeIndex(ctx, t, subber, db, chain)
	before := ssfeeIndexEntries(t, db)

	ssFee3 := newTestSSFee(t, 3, nullSSFeeInput, testSSFeePayTo)
	bk3 := addSSFeeBlock(t, chain, &g, "bk3",
		newTestSSFee(t, 3, nullSSFeeInput, nil), ssFee3)
	if err := subber.CatchUp(ctx, db, chain); err != nil {
		t.Fatal(err)
	}
	assertSSFeeIndexEntries(t, db, map[string][]wire.OutPoint{
		ssfeeIndexEntryKey(t, ssFee2): {ssFeePayment(ssFee2), ssFeePayment(ssFee3)},
	})

	// Replace bk3 and restart the index, which disconnects bk3.
	g.SetTip("bk2")
	if err := chain.RemoveBlock(bk3); err != nil {
		t.Fatal(err)
	}
	bk3a := addBlock(t, chain, &g, "bk3a")
	stopSSFeeIndex(t, subber, idx)
	idx = startSSFeeIndex(ctx, t, subber, db, chain)
	assertSSFeeIndexTip(t, idx, bk3a)
	assertSSFeeIndexEntries(t, db, before)
}

// TestSSFeeIndexAugmentedSSFeeWithoutPayment records what the SSFee index holds
// when an SSFee transaction without a payment output spends an indexed output:
// the index skips it, so it keeps the spent output while the block is
// connected and does not add it again when the block is disconnected.
func TestSSFeeIndexAugmentedSSFeeWithoutPayment(t *testing.T) {
	db := setupDB(t)
	chain, err := newTestChain()
	if err != nil {
		t.Fatal(err)
	}
	g, err := chaingen.MakeGenerator(chaincfg.SimNetParams())
	if err != nil {
		t.Fatal(err)
	}

	// Create the following chain, where bk3 and bk3a both spend the payment
	// of bk2 and only bk3 has a payment output itself:
	//
	//   genesis -> bk1 -> bk2 -> bk3
	//                        \-> bk3a
	//                        \-> bk3b
	addBlock(t, chain, &g, "bk1")
	ssFee2 := newTestSSFee(t, 2, nullSSFeeInput, testSSFeePayTo)
	addSSFeeBlock(t, chain, &g, "bk2", ssFee2)
	key := ssfeeIndexEntryKey(t, ssFee2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subber := NewIndexSubscriber(ctx)
	go subber.Run(ctx)
	idx := startSSFeeIndex(ctx, t, subber, db, chain)
	assertSSFeeIndexEntries(t, db, map[string][]wire.OutPoint{
		key: {ssFeePayment(ssFee2)},
	})

	// An augmenting SSFee with a payment replaces the output it spends.
	ssFee3 := newTestSSFee(t, 3, ssFeePayment(ssFee2), testSSFeePayTo)
	bk3 := addSSFeeBlock(t, chain, &g, "bk3", ssFee3)
	if err := subber.CatchUp(ctx, db, chain); err != nil {
		t.Fatal(err)
	}
	assertSSFeeIndexEntries(t, db, map[string][]wire.OutPoint{
		key: {ssFeePayment(ssFee3)},
	})

	// An augmenting SSFee without a payment is skipped, so the output it
	// spends stays in the index.
	g.SetTip("bk2")
	if err := chain.RemoveBlock(bk3); err != nil {
		t.Fatal(err)
	}
	bk3a := addSSFeeBlock(t, chain, &g, "bk3a",
		newTestSSFee(t, 3, ssFeePayment(ssFee2), nil))
	stopSSFeeIndex(t, subber, idx)
	idx = startSSFeeIndex(ctx, t, subber, db, chain)
	assertSSFeeIndexTip(t, idx, bk3a)
	assertSSFeeIndexEntries(t, db, map[string][]wire.OutPoint{
		key: {ssFeePayment(ssFee2)},
	})

	// Disconnecting it does not add the spent output a second time.
	g.SetTip("bk2")
	if err := chain.RemoveBlock(bk3a); err != nil {
		t.Fatal(err)
	}
	bk3b := addBlock(t, chain, &g, "bk3b")
	stopSSFeeIndex(t, subber, idx)
	idx = startSSFeeIndex(ctx, t, subber, db, chain)
	assertSSFeeIndexTip(t, idx, bk3b)
	assertSSFeeIndexEntries(t, db, map[string][]wire.OutPoint{
		key: {ssFeePayment(ssFee2)},
	})
}

// newTestSSFee returns a miner SSFee transaction for the given height that
// spends prevOut. It only has its marker when payTo is nil, and otherwise pays
// to the OP_SSGEN-tagged P2PKH script of payTo at output 1.
func newTestSSFee(t *testing.T, height int64, prevOut wire.OutPoint, payTo []byte) *wire.MsgTx {
	t.Helper()
	ssFee := wire.NewMsgTx()
	ssFee.Version = 3
	ssFee.AddTxIn(&wire.TxIn{
		PreviousOutPoint: prevOut,
		Sequence:         wire.MaxTxInSequenceNum,
		BlockHeight:      wire.NullBlockHeight,
		BlockIndex:       wire.NullBlockIndex,
	})
	ssFee.AddTxOut(&wire.TxOut{CoinType: 1, SKAValue: new(big.Int),
		PkScript: stake.CreateMinerSSFeeMarker(height)})
	if payTo != nil {
		pkScript, err := stake.ConsolidationAddrToPkScript(payTo)
		if err != nil {
			t.Fatal(err)
		}
		ssFee.AddTxOut(&wire.TxOut{CoinType: 1, SKAValue: big.NewInt(1),
			PkScript: pkScript})
	}
	if !stake.IsSSFee(ssFee) {
		t.Fatal("test transaction is not an SSFee transaction")
	}
	return ssFee
}

// ssFeePayment returns the outpoint of the payment output of the given SSFee
// transaction.
func ssFeePayment(ssFee *wire.MsgTx) wire.OutPoint {
	return wire.OutPoint{Hash: ssFee.TxHash(), Index: 1, Tree: wire.TxTreeStake}
}

// ssfeeIndexEntryKey returns the hex-encoded SSFee index key of the payment of
// the given SSFee transaction to testSSFeePayTo.
func ssfeeIndexEntryKey(t *testing.T, ssFee *wire.MsgTx) string {
	t.Helper()
	key, err := makeSSFeeIndexKey(extractFeeTypeFromSSFee(ssFee),
		ssFee.TxOut[1].CoinType, testSSFeePayTo)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(key)
}

// addSSFeeBlock extends the provided chain with a generated block holding the
// given stake transactions.
func addSSFeeBlock(t *testing.T, chain *testChain, g *chaingen.Generator, name string, stxs ...*wire.MsgTx) *dcrutil.Block {
	t.Helper()
	blk := dcrutil.NewBlock(g.NextBlock(name, nil, nil, func(b *wire.MsgBlock) {
		for _, stx := range stxs {
			b.AddSTransaction(stx)
		}
	}))
	if err := chain.AddBlock(blk); err != nil {
		t.Fatal(err)
	}
	return blk
}

// startSSFeeIndex creates the SSFee index and catches it up to the tip of the
// chain.
func startSSFeeIndex(ctx context.Context, t *testing.T, subber *IndexSubscriber, db database.DB, chain *testChain) *SSFeeIndex {
	t.Helper()
	idx, err := NewSSFeeIndex(subber, db, chain)
	if err != nil {
		t.Fatal(err)
	}
	if err := subber.CatchUp(ctx, db, chain); err != nil {
		t.Fatal(err)
	}
	return idx
}

// stopSSFeeIndex stops the subscription of the SSFee index.
func stopSSFeeIndex(t *testing.T, subber *IndexSubscriber, idx *SSFeeIndex) {
	t.Helper()
	subber.mtx.Lock()
	err := idx.sub.stop()
	subber.mtx.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

// ssfeeIndexEntries returns the outpoints the SSFee index holds by
// hex-encoded key.
func ssfeeIndexEntries(t *testing.T, db database.DB) map[string][]wire.OutPoint {
	t.Helper()
	entries := make(map[string][]wire.OutPoint)
	err := db.View(func(dbTx database.Tx) error {
		bucket := dbTx.Metadata().Bucket(ssfeeIndexKey)
		return bucket.ForEach(func(k, v []byte) error {
			outpoints, err := deserializeOutPoints(v)
			if err != nil {
				return err
			}
			entries[hex.EncodeToString(k)] = outpoints
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// assertSSFeeIndexEntries fails the test unless the SSFee index holds exactly
// the given outpoints by hex-encoded key.
func assertSSFeeIndexEntries(t *testing.T, db database.DB, want map[string][]wire.OutPoint) {
	t.Helper()
	if got := ssfeeIndexEntries(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("index entries are %v, want %v", got, want)
	}
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
