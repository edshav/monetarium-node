// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package mining

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/wire"
)

// TestIsSSFeeOutputOfType ensures the template generator reads the type of the
// SSFee transaction that created an output from the transaction itself, the
// way consensus does, including for transactions whose marker or output order
// the SSFee index misreads.
func TestIsSSFeeOutputOfType(t *testing.T) {
	const ska1 = cointype.CoinType(1)
	const height = 17997 // little-endian height bytes 0x4d 0x46 read "MF"

	payScript, err := stake.ConsolidationAddrToPkScript(bytes.Repeat([]byte{0x11}, 20))
	if err != nil {
		t.Fatal(err)
	}
	newTx := func(scripts ...[]byte) *wire.MsgTx {
		tx := wire.NewMsgTx()
		tx.Version = wire.TxVersionTreasury
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex},
		})
		for _, script := range scripts {
			tx.AddTxOut(&wire.TxOut{CoinType: ska1, SKAValue: big.NewInt(1),
				PkScript: script})
		}
		return tx
	}

	// A staker SSFee mined at a height whose marker bytes contain "MF", and a
	// miner SSFee whose marker is not its first output.
	staker := newTx(stake.CreateStakerSSFeeMarker(height, 0), payScript)
	miner := newTx(payScript, stake.CreateMinerSSFeeMarker(height))
	block := dcrutil.NewBlock(&wire.MsgBlock{
		Header:        wire.BlockHeader{Height: height},
		STransactions: []*wire.MsgTx{staker, miner},
	})
	g := &BlkTmplGenerator{cfg: &Config{
		BlockByHeight: func(h int64) (*dcrutil.Block, error) {
			if h != height {
				t.Fatalf("fetched block at height %d, want %d", h, height)
			}
			return block, nil
		},
	}}

	tests := []struct {
		name     string
		outpoint wire.OutPoint
		index    uint32
		feeType  string
	}{{
		name:     "staker SSFee at a height that reads MF",
		outpoint: wire.OutPoint{Hash: staker.TxHash(), Index: 1, Tree: wire.TxTreeStake},
		index:    0,
		feeType:  "SF",
	}, {
		name:     "miner SSFee with the marker last",
		outpoint: wire.OutPoint{Hash: miner.TxHash(), Index: 0, Tree: wire.TxTreeStake},
		index:    1,
		feeType:  "MF",
	}}
	for _, test := range tests {
		if !g.isSSFeeOutputOfType(&test.outpoint, height, test.index, test.feeType) {
			t.Errorf("%s: output not recognised as %s", test.name, test.feeType)
		}
	}
}
