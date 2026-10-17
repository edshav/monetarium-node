// Copyright (c) 2020-2022 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"math/big"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/chaingen"
	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/wire"
)

// TestFetchUtxoView ensures fetching a utxo viewpoint for the current tip block
// works as intended for various combinations of disapproval states for the tip
// block itself as well as when the tip block disapproves its parent.
func TestFetchUtxoView(t *testing.T) {
	const (
		// voteBitNo and voteBitYes represent no and yes votes, respectively, on
		// whether or not to approve the previous block.
		voteBitNo  = 0x0000
		voteBitYes = 0x0001
	)

	// Create a test harness initialized with the genesis block as the tip.
	params := chaincfg.RegNetParams()
	g := newChaingenHarness(t, params)

	// ---------------------------------------------------------------------
	// Create some convenience functions to improve test readability.
	// ---------------------------------------------------------------------

	// fetchUtxoViewTipApproved calls FetchUtxoView method with the flag set to
	// indicate that the transactions in the regular transaction tree of the tip
	// should be included.
	fetchUtxoViewTipApproved := func(tx *dcrutil.Tx) (*UtxoViewpoint, error) {
		return g.chain.FetchUtxoView(tx, true)
	}

	// fetchUtxoViewTipDisapproved calls FetchUtxoView method with the flag set
	// to indicate that the transactions in the regular transaction tree of the
	// tip should NOT be included.
	fetchUtxoViewTipDisapproved := func(tx *dcrutil.Tx) (*UtxoViewpoint, error) {
		return g.chain.FetchUtxoView(tx, false)
	}

	// testInputsSpent checks the provided view considers all of the transaction
	// outputs referenced by the inputs of the provided transaction spent
	// according to the given flag.
	testInputsSpent := func(view *UtxoViewpoint, tx *dcrutil.Tx, spent bool) {
		t.Helper()

		for txInIdx, txIn := range tx.MsgTx().TxIn {
			prevOut := &txIn.PreviousOutPoint
			entry := view.LookupEntry(*prevOut)
			gotSpent := entry == nil || entry.IsSpent()
			if gotSpent != spent {
				t.Fatalf("unexpected spent state for txo %s referenced by "+
					"input %d -- got %v, want %v", prevOut, txInIdx,
					gotSpent, spent)
			}
		}
	}

	// ---------------------------------------------------------------------
	// Generate and accept enough blocks to reach stake validation height.
	// ---------------------------------------------------------------------

	g.AdvanceToStakeValidationHeight()

	// ---------------------------------------------------------------------
	// Create block that has a transaction available in its regular tx tree
	// to use as a base for the tests below.
	//
	//   ... -> b0
	// ---------------------------------------------------------------------

	outs := g.OldestCoinbaseOuts()
	b0 := g.NextBlock("b0", &outs[0], outs[1:])
	g.AcceptTipBlock()

	// Create a transaction that spends from an output in the regular tx tree of
	// the tip block.
	fee := dcrutil.Amount(1)
	b0Tx1Out0 := chaingen.MakeSpendableOut(b0, 1, 0)
	spendB0Tx1Out0 := dcrutil.NewTx(g.CreateSpendTx(&b0Tx1Out0, fee))

	// Ensure that fetching a view for a transaction that spends an output in
	// the regular tx tree of the tip block does NOT consider the referenced
	// output spent when the regular tx tree of the tip block is treated as
	// approved.
	view, err := fetchUtxoViewTipApproved(spendB0Tx1Out0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	testInputsSpent(view, spendB0Tx1Out0, false)

	// Ensure that fetching a view for a transaction that spends an output in
	// the regular tx tree of the tip block considers the referenced output
	// spent when the regular tx tree of the tip block is treated as disapproved
	// since they should be treated as if they do NOT exist.
	view, err = fetchUtxoViewTipDisapproved(spendB0Tx1Out0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	testInputsSpent(view, spendB0Tx1Out0, true)

	// ---------------------------------------------------------------------
	// Create block that disapproves the regular tx tree of the prev block
	// and does NOT include any disapproved regular transactions from it.
	//
	//   ... -> b0 -> b1 (disapproves b0)
	// ---------------------------------------------------------------------

	outs = g.OldestCoinbaseOuts()
	g.NextBlock("b1", &outs[0], outs[1:], func(b *wire.MsgBlock) {
		b.Header.VoteBits &^= voteBitYes
		g.ReplaceVoteBits(voteBitNo)(b)
	})
	g.AssertTipDisapprovesPrevious()
	g.AcceptTipBlock()

	// Ensure that fetching a view for a transaction that is in the regular tx
	// tree of a disapproved block prior to the tip block when the tip block
	// does NOT otherwise spend those outputs does NOT consider the referenced
	// outputs spent (because the spend was undone by the disapproval)
	// regardless of whether or not the regular tx tree of the tip block is
	// treated as approved or disapproved.
	b0Tx1 := dcrutil.NewTx(b0.Transactions[1])
	view, err = fetchUtxoViewTipApproved(b0Tx1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	testInputsSpent(view, b0Tx1, false)

	view, err = fetchUtxoViewTipDisapproved(b0Tx1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	testInputsSpent(view, b0Tx1, false)

	// Ensure that fetching a view for a transaction that spends an output in
	// the regular tx tree of a disapproved block prior to the tip block when
	// the tip block does NOT otherwise spend the output considers the output
	// spent (since it was undone by the disapproval and therefore does NOT
	// exist) regardless of whether or not the regular tx tree of the tip block
	// is treated as approved or disapproved.
	view, err = fetchUtxoViewTipApproved(spendB0Tx1Out0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	testInputsSpent(view, spendB0Tx1Out0, true)

	view, err = fetchUtxoViewTipDisapproved(spendB0Tx1Out0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	testInputsSpent(view, spendB0Tx1Out0, true)

	// ---------------------------------------------------------------------
	// Create block that disapproves the regular tx tree of the prev block
	// and also includes one of those disapproved regular transactions and
	// then reorganize the chain to it.
	//
	//   ... -> b0 -> b1a (disapproves b0)
	//            \-> b1 (disapproves b0)
	// ---------------------------------------------------------------------

	g.SetTip("b0")
	b1a := g.NextBlock("b1a", &outs[0], outs[1:], func(b *wire.MsgBlock) {
		b.Header.VoteBits &^= voteBitYes
		g.ReplaceVoteBits(voteBitNo)(b)

		b.Transactions[1] = b0Tx1.MsgTx()
	})
	g.AssertTipDisapprovesPrevious()
	g.AcceptedToSideChainWithExpectedTip("b1")
	g.ForceTipReorg("b1", "b1a")

	// Ensure that fetching a view for a transaction that is in the regular tx
	// tree of a disapproved block prior to the tip block and is also included
	// in the tip block itself considers the referenced outputs spent when the
	// regular tx tree of the tip block is treated as approved.
	view, err = fetchUtxoViewTipApproved(b0Tx1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	testInputsSpent(view, b0Tx1, true)

	// Ensure that fetching a view for a transaction that is in the regular tx
	// tree of a disapproved block prior to the tip block and is also included
	// in the tip block itself does NOT consider the referenced outputs spent
	// when the regular tx tree of the tip block is treated as disapproved since
	// both instances of the transaction are disapproved.
	view, err = fetchUtxoViewTipDisapproved(b0Tx1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	testInputsSpent(view, b0Tx1, false)

	// Create a transaction that spends from an output in the regular tx tree of
	// the tip block.  Note that this is spending from the same transaction that
	// was disapproved in the block prior to the current tip and included in the
	// tip again.
	b1aTx1Out0 := chaingen.MakeSpendableOut(b1a, 1, 0)
	spendB1aTx1Out0 := dcrutil.NewTx(g.CreateSpendTx(&b1aTx1Out0, fee))

	// Ensure that fetching a view for a transaction that spends an output in
	// the regular tx tree of the tip block that was also included in the block
	// the tip disapproves does NOT consider the referenced output spent when
	// the regular tx tree of the tip block is treated as approved.
	view, err = fetchUtxoViewTipApproved(spendB1aTx1Out0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	testInputsSpent(view, spendB1aTx1Out0, false)

	// Ensure that fetching a view for a transaction that spends an output in
	// the regular tx tree of the tip block that was also included in the block
	// the tip disapproves consider the referenced output spent when the regular
	// tx tree of the tip block is treated as disapproved.
	view, err = fetchUtxoViewTipDisapproved(spendB1aTx1Out0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	testInputsSpent(view, spendB1aTx1Out0, true)
}

// TestUtxoViewpointCoinTypeTracking tests that UTXO entries correctly preserve
// coin types when added through addTxOut and disconnectTransactions methods.
func TestUtxoViewpointCoinTypeTracking(t *testing.T) {
	// Create a new UTXO viewpoint
	view := NewUtxoViewpoint(nil)

	// Create test transaction outputs with different coin types
	// Use proper P2PKH script (OP_DUP OP_HASH160 <20-byte hash> OP_EQUALVERIFY OP_CHECKSIG)
	p2pkhScript := []byte{
		0x76,                                                       // OP_DUP
		0xa9,                                                       // OP_HASH160
		0x14,                                                       // Push 20 bytes
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, // 20-byte hash
		0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x11, 0x22, 0x33, 0x44, 0x55,
		0x88, // OP_EQUALVERIFY
		0xac, // OP_CHECKSIG
	}

	varTxOut := &wire.TxOut{
		Value:    1000000000, // 10 VAR
		Version:  0,
		PkScript: p2pkhScript,
		CoinType: cointype.CoinTypeVAR,
	}

	skaTxOut := &wire.TxOut{
		Value:    0,                      // Not used for SKA
		SKAValue: big.NewInt(5000000000), // 50 SKA - uses SKAValue
		Version:  0,
		PkScript: p2pkhScript,
		CoinType: cointype.CoinType(1),
	}

	// Create outpoints for the test UTXOs
	varOutpoint := wire.OutPoint{
		Hash:  [32]byte{1, 2, 3, 4},
		Index: 0,
		Tree:  wire.TxTreeRegular,
	}

	skaOutpoint := wire.OutPoint{
		Hash:  [32]byte{5, 6, 7, 8},
		Index: 0,
		Tree:  wire.TxTreeRegular,
	}

	// Test addTxOut method preserves coin types
	flags := encodeUtxoFlags(false, false, 0) // not coinbase, no expiry, regular tx
	view.addTxOut(varOutpoint, varTxOut, flags, 100, 0, nil)
	view.addTxOut(skaOutpoint, skaTxOut, flags, 100, 1, nil)

	// Verify VAR UTXO entry has correct coin type
	varEntry := view.LookupEntry(varOutpoint)
	if varEntry == nil {
		t.Fatal("VAR UTXO entry not found")
	}
	if varEntry.CoinType() != cointype.CoinTypeVAR {
		t.Errorf("Expected VAR coin type (%d), got %d", cointype.CoinTypeVAR, varEntry.CoinType())
	}
	if varEntry.Amount() != 1000000000 {
		t.Errorf("Expected VAR amount 1000000000, got %d", varEntry.Amount())
	}

	// Verify SKA UTXO entry has correct coin type
	skaEntry := view.LookupEntry(skaOutpoint)
	if skaEntry == nil {
		t.Fatal("SKA UTXO entry not found")
	}
	if skaEntry.CoinType() != cointype.CoinType(1) {
		t.Errorf("Expected SKA coin type (%d), got %d", cointype.CoinType(1), skaEntry.CoinType())
	}
	// SKA amounts use SKAAmount(), not Amount()
	skaAmt := skaEntry.SKAAmount()
	if skaAmt == nil || skaAmt.Int64() != 5000000000 {
		var got int64
		if skaAmt != nil {
			got = skaAmt.Int64()
		}
		t.Errorf("Expected SKA amount 5000000000, got %d", got)
	}

	// Test AmountWithCoinType method for VAR
	varAmount, varCoinType := varEntry.AmountWithCoinType()
	if varAmount != 1000000000 || varCoinType != cointype.CoinTypeVAR {
		t.Errorf("AmountWithCoinType for VAR: expected (1000000000, %d), got (%d, %d)",
			cointype.CoinTypeVAR, varAmount, varCoinType)
	}

	// For SKA, AmountWithCoinType returns 0 for amount (uses SKAAmount instead)
	skaAmountFromMethod, skaCoinType := skaEntry.AmountWithCoinType()
	if skaCoinType != cointype.CoinType(1) {
		t.Errorf("AmountWithCoinType for SKA: expected coin type %d, got %d",
			cointype.CoinType(1), skaCoinType)
	}
	// SKA's Amount() returns 0 since we use SKAValue; this is expected
	if skaAmountFromMethod != 0 {
		t.Errorf("AmountWithCoinType for SKA: expected 0 (SKA uses SKAAmount), got %d", skaAmountFromMethod)
	}

	// Test enhanced query methods
	// Test LookupEntriesByCoinType
	varEntries := view.LookupEntriesByCoinType(cointype.CoinTypeVAR)
	if len(varEntries) != 1 {
		t.Errorf("Expected 1 VAR entry, got %d", len(varEntries))
	}
	if varEntries[varOutpoint] == nil {
		t.Error("VAR entry not found in filtered results")
	}

	skaEntries := view.LookupEntriesByCoinType(cointype.CoinType(1))
	if len(skaEntries) != 1 {
		t.Errorf("Expected 1 SKA entry, got %d", len(skaEntries))
	}
	if skaEntries[skaOutpoint] == nil {
		t.Error("SKA entry not found in filtered results")
	}

	// Test GetCoinTypeBalance
	varBalance := view.GetCoinTypeBalance(cointype.CoinTypeVAR)
	if varBalance != 1000000000 {
		t.Errorf("Expected VAR balance 1000000000, got %d", varBalance)
	}

	skaBalance := view.GetCoinTypeBalance(cointype.CoinType(1))
	if skaBalance != 5000000000 {
		t.Errorf("Expected SKA balance 5000000000, got %d", skaBalance)
	}

	// Test GetCoinTypeCount
	varCount := view.GetCoinTypeCount(cointype.CoinTypeVAR)
	if varCount != 1 {
		t.Errorf("Expected VAR count 1, got %d", varCount)
	}

	skaCount := view.GetCoinTypeCount(cointype.CoinType(1))
	if skaCount != 1 {
		t.Errorf("Expected SKA count 1, got %d", skaCount)
	}

	// Test with non-existent coin type
	unknownEntries := view.LookupEntriesByCoinType(cointype.CoinType(99))
	if len(unknownEntries) != 0 {
		t.Errorf("Expected 0 entries for unknown coin type, got %d", len(unknownEntries))
	}

	unknownBalance := view.GetCoinTypeBalance(cointype.CoinType(99))
	if unknownBalance != 0 {
		t.Errorf("Expected 0 balance for unknown coin type, got %d", unknownBalance)
	}

	unknownCount := view.GetCoinTypeCount(cointype.CoinType(99))
	if unknownCount != 0 {
		t.Errorf("Expected 0 count for unknown coin type, got %d", unknownCount)
	}
}

// TestSameBlockStakeSpend ensures stake transactions, including augmented SSFee
// transactions which are exempt from the stake output maturity rules, are never
// able to spend outputs created in the same block, while regular transactions
// still are.
func TestSameBlockStakeSpend(t *testing.T) {
	// Create a test harness initialized with the genesis block as the tip.
	params := chaincfg.RegNetParams()
	g := newChaingenHarness(t, params)
	g.AdvanceToStakeValidationHeight()

	// sameBlockSpend returns a munger that adds a transaction to the regular
	// tree followed by a regular transaction that spends its first output and,
	// when provided, invokes the given func to add a stake transaction that
	// spends the same output.  The transactions pay no fee so the generator
	// does not regenerate the SSFee transactions.
	outs := g.OldestCoinbaseOuts()
	sameBlockSpend := func(addStakeSpend func(b *wire.MsgBlock,
		originTx *wire.MsgTx, originIdx uint32)) func(*wire.MsgBlock) {

		return func(b *wire.MsgBlock) {
			originTx := g.CreateSpendTx(&outs[0], 0)
			b.AddTransaction(originTx)
			originIdx := uint32(len(b.Transactions) - 1)
			b.AddTransaction(g.CreateSpendTxForTx(originTx, b.Header.Height,
				originIdx, 0))
			if addStakeSpend != nil {
				addStakeSpend(b, originTx, originIdx)
			}
		}
	}

	// Ensure a ticket spending a same-block output that a regular transaction
	// also spends is rejected since it would otherwise be a double spend.
	//
	//   ... -> bsvh
	//             \-> bss0
	baseName := g.TipName()
	g.NextBlock("bss0", nil, outs[1:], sameBlockSpend(func(b *wire.MsgBlock,
		originTx *wire.MsgTx, originIdx uint32) {

		spend := chaingen.MakeSpendableOutForTx(originTx, b.Header.Height,
			originIdx, 0)
		ticketPrice := dcrutil.Amount(g.CalcNextRequiredStakeDifficulty())
		b.AddSTransaction(g.CreateTicketPurchaseTx(&spend, ticketPrice, 0))
		b.Header.FreshStake++
	}))
	g.RejectTipBlock(ErrMissingTxOut)

	// Ensure an augmented SSFee spending the payout of an SSFee earlier in the
	// same block is rejected as well.  An augmented SSFee may only spend an
	// SSFee output, so that is the only same-block output it could spend.
	//
	//   ... -> bsvh
	//             \-> bss1
	g.SetTip(baseName)
	g.NextBlock("bss1", nil, outs[1:], func(b *wire.MsgBlock) {
		for i, stx := range b.STransactions {
			if stake.DetermineTxType(stx) != stake.TxTypeSSFee {
				continue
			}
			const paymentIdx = 0
			amount := stx.TxOut[paymentIdx].Value
			augmented := stx.Copy()
			augmented.TxIn[0] = &wire.TxIn{
				PreviousOutPoint: wire.OutPoint{Hash: stx.TxHash(),
					Index: paymentIdx, Tree: wire.TxTreeStake},
				Sequence:    wire.MaxTxInSequenceNum,
				ValueIn:     amount,
				BlockHeight: b.Header.Height,
				BlockIndex:  uint32(i),
			}
			augmented.TxOut[paymentIdx].Value += amount
			b.AddSTransaction(augmented)
			return
		}
		t.Fatal("block template has no SSFee transaction to augment")
	})
	g.RejectTipBlock(ErrMissingTxOut)

	// Ensure regular transactions are still allowed to spend outputs of an
	// earlier transaction in the same block.
	//
	//   ... -> bsvh -> bss2
	g.SetTip(baseName)
	g.NextBlock("bss2", nil, outs[1:], sameBlockSpend(nil))
	g.AcceptTipBlock()
}

// TestAddRegularInputUtxosOnlyReferenced ensures that only the outputs of
// in-flight regular transactions that are actually referenced by later regular
// transactions in the same block are added to the view.
func TestAddRegularInputUtxosOnlyReferenced(t *testing.T) {
	origin := wire.NewMsgTx()
	origin.AddTxOut(wire.NewTxOut(10, []byte{0x51}))
	origin.AddTxOut(wire.NewTxOut(20, []byte{0x51}))
	originHash := origin.TxHash()
	spender := wire.NewMsgTx()
	spender.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{
		Hash:  originHash,
		Index: 1,
	}})
	block := dcrutil.NewBlock(&wire.MsgBlock{
		Header:       wire.BlockHeader{Height: 100},
		Transactions: []*wire.MsgTx{wire.NewMsgTx(), origin, spender},
	})

	view := NewUtxoViewpoint(nil)
	view.addRegularInputUtxos(block, true)

	referenced := wire.OutPoint{Hash: originHash, Index: 1}
	if entry := view.LookupEntry(referenced); entry == nil || entry.IsSpent() {
		t.Fatalf("referenced in-flight output %v not available in view",
			referenced)
	}
	unreferenced := wire.OutPoint{Hash: originHash, Index: 0}
	if entry := view.LookupEntry(unreferenced); entry != nil {
		t.Fatalf("unreferenced in-flight output %v added to view", unreferenced)
	}
}
