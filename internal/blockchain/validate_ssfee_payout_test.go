// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"encoding/hex"
	"math/big"
	"sort"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/blockchain/standalone"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/txscript"
	"github.com/monetarium/monetarium-node/wire"
)

// ssFeeTestVote returns a vote with one reward output whose consolidation
// address is the given hash160.
func ssFeeTestVote(t *testing.T, consolidation []byte) *dcrutil.Tx {
	t.Helper()
	rewardScript, err := stake.ConsolidationAddrToPkScript(consolidation)
	if err != nil {
		t.Fatal(err)
	}
	vote := wire.NewMsgTx()
	vote.AddTxOut(&wire.TxOut{PkScript: []byte{txscript.OP_RETURN}})
	vote.AddTxOut(&wire.TxOut{PkScript: []byte{txscript.OP_RETURN}})
	vote.AddTxOut(&wire.TxOut{Value: 1e8, PkScript: rewardScript})
	out, err := stake.CreateSSFeeConsolidationOutput(consolidation)
	if err != nil {
		t.Fatal(err)
	}
	vote.AddTxOut(out)
	return dcrutil.NewTx(vote)
}

// ssFeeTestTx returns a null-input SSFee transaction with the given marker that
// pays amount of the given coin type to payScript, laid out as the block
// template generator does.
func ssFeeTestTx(coinType cointype.CoinType, marker, payScript []byte,
	amount *big.Int) *wire.MsgTx {

	tx := wire.NewMsgTx()
	tx.Version = 3
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex},
		Sequence:         wire.MaxTxInSequenceNum,
		BlockHeight:      wire.NullBlockHeight,
		BlockIndex:       wire.NullBlockIndex,
	})
	markerOut := &wire.TxOut{CoinType: coinType, PkScript: marker}
	payOut := &wire.TxOut{CoinType: coinType, PkScript: payScript}
	if coinType.IsSKA() {
		markerOut.SKAValue = new(big.Int)
		payOut.SKAValue = amount
	} else {
		payOut.Value = amount.Int64()
	}
	tx.AddTxOut(markerOut)
	tx.AddTxOut(payOut)
	return tx
}

// stakerSSFeeTestTxns returns one staker SSFee transaction per consolidation
// address of the votes, each paying an equal share per vote and the first
// address in sorted order the remainder.
func stakerSSFeeTestTxns(t *testing.T, coinType cointype.CoinType,
	votes []*dcrutil.Tx, total *big.Int) []*wire.MsgTx {

	t.Helper()
	counts := make(map[string]int64)
	for _, vote := range votes {
		hash160, err := stake.ExtractSSFeeConsolidationAddr(vote.MsgTx())
		if err != nil {
			t.Fatal(err)
		}
		counts[hex.EncodeToString(hash160)]++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	numVotes := big.NewInt(int64(len(votes)))
	perVote := new(big.Int).Div(total, numVotes)
	remainder := new(big.Int).Sub(total, new(big.Int).Mul(perVote, numVotes))
	txns := make([]*wire.MsgTx, 0, len(keys))
	for i, key := range keys {
		amount := new(big.Int).Mul(perVote, big.NewInt(counts[key]))
		if i == 0 {
			amount.Add(amount, remainder)
		}
		hash160, _ := hex.DecodeString(key)
		payScript, err := stake.ConsolidationAddrToPkScript(hash160)
		if err != nil {
			t.Fatal(err)
		}
		marker := stake.CreateStakerSSFeeMarker(200, uint16(i))
		txns = append(txns, ssFeeTestTx(coinType, marker, payScript, amount))
	}
	return txns
}

// TestSSFeeAcceptsTemplatePayouts ensures SSFee transactions laid out as the
// block template generator does, one payout each, distribute the fees of a
// block with one vote and with votes of several consolidation addresses.
func TestSSFeeAcceptsTemplatePayouts(t *testing.T) {
	addrA := []byte{1: 0xaa, 19: 0x01}
	addrB := []byte{1: 0xbb, 19: 0x02}
	addrC := []byte{1: 0xcc, 19: 0x03}
	minerScript, err := stake.ConsolidationAddrToPkScript(addrC)
	if err != nil {
		t.Fatal(err)
	}
	work, stakeShare, _, _ := standalone.GetSubsidyProportions(
		standalone.SSVMonetarium)

	tests := []struct {
		name     string
		coinType cointype.CoinType
		votes    [][]byte
		fee      int64
	}{{
		name:     "VAR fees, one vote",
		coinType: cointype.CoinTypeVAR,
		votes:    [][]byte{addrA},
		fee:      2_007,
	}, {
		name:     "VAR fees, five votes of three addresses",
		coinType: cointype.CoinTypeVAR,
		votes:    [][]byte{addrA, addrB, addrA, addrC, addrB},
		fee:      2_007,
	}, {
		name:     "SKA1 fees, one vote",
		coinType: 1,
		votes:    [][]byte{addrB},
		fee:      1_000_000_000_000_000_007,
	}, {
		name:     "SKA1 fees, five votes of three addresses",
		coinType: 1,
		votes:    [][]byte{addrC, addrA, addrB, addrA, addrC},
		fee:      1_000_000_000_000_000_007,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			votes := make([]*dcrutil.Tx, 0, len(test.votes))
			for _, addr := range test.votes {
				votes = append(votes, ssFeeTestVote(t, addr))
			}
			fees := wire.NewFeesByType()
			fees.AddBig(test.coinType, big.NewInt(test.fee))
			minerFees, stakerFees := wire.CalcFeeSplitByCoinType(fees, work,
				stakeShare)

			ssFeeTxns := stakerSSFeeTestTxns(t, test.coinType, votes,
				stakerFees.GetBig(test.coinType))
			if test.coinType.IsSKA() {
				ssFeeTxns = append(ssFeeTxns, ssFeeTestTx(test.coinType,
					stake.CreateMinerSSFeeMarker(200), minerScript,
					minerFees.GetBig(test.coinType)))
			}

			err := (&BlockChain{}).validateSSFeeTxns(nil, nil, votes,
				ssFeeTxns, fees, standalone.SSVMonetarium)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
