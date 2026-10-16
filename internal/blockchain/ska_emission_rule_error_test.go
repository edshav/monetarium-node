// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrec/secp256k1"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/txscript"
	"github.com/monetarium/monetarium-node/txscript/stdaddr"
	"github.com/monetarium/monetarium-node/wire"
)

// newSKAEmissionRuleHarness returns a regnet harness advanced to stake
// validation height whose SKA1 emission window opens emitOffset blocks after
// stake validation height and stays open for window blocks, along with a
// function that returns an SKA1 emission transaction for a given block height
// signed with the given nonce.
func newSKAEmissionRuleHarness(t *testing.T, emitOffset, window int64) (*chaingenHarness,
	func(height int64, nonce uint64) *wire.MsgTx) {

	t.Helper()

	params := cloneParams(chaincfg.RegNetParams())
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("failed to generate emission key: %v", err)
	}
	addr, err := stdaddr.NewAddressScriptHashV0([]byte{txscript.OP_TRUE},
		params)
	if err != nil {
		t.Fatalf("failed to create p2sh OP_TRUE address: %v", err)
	}
	amount := new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))
	params.SKACoins = map[cointype.CoinType]*chaincfg.SKACoinConfig{
		1: {
			CoinType:          1,
			Name:              "Skarb-1",
			Symbol:            "SKA1",
			MaxSupply:         amount,
			AtomsPerCoin:      big.NewInt(1e18),
			EmissionHeight:    int32(params.StakeValidationHeight + emitOffset),
			EmissionWindow:    int32(window),
			Active:            true,
			EmissionAddresses: []string{addr.String()},
			EmissionAmounts:   []*big.Int{amount},
			EmissionKey:       priv.PubKey(),
		},
	}
	params.InitialSKATypes = []cointype.CoinType{1}

	g := newChaingenHarness(t, params)
	g.AdvanceToStakeValidationHeight()

	emission := func(height int64, nonce uint64) *wire.MsgTx {
		tx := createTestEmissionTx(t, params.SKACoins[1].EmissionAddresses,
			[]*big.Int{amount}, 1, params)
		signEmissionTx(t, tx, &chaincfg.SKAEmissionAuth{
			EmissionKey: priv.PubKey(),
			CoinType:    1,
			Nonce:       nonce,
			Amount:      amount,
			Height:      height,
		}, priv, params)
		tx.TxIn[0].BlockHeight = wire.NullBlockHeight
		tx.TxIn[0].BlockIndex = wire.NullBlockIndex
		return tx
	}
	return g, emission
}

// expectKnownInvalid ensures the named block is or is not marked invalid in the
// block index.
func expectKnownInvalid(g *chaingenHarness, blockName string, want bool) {
	g.t.Helper()

	hash := g.BlockByName(blockName).BlockHash()
	node := g.chain.index.LookupNode(&hash)
	if node == nil {
		g.t.Fatalf("block %q is not in the block index", blockName)
	}
	if got := g.chain.index.NodeStatus(node).KnownInvalid(); got != want {
		g.t.Fatalf("block %q known invalid: got %v, want %v", blockName, got,
			want)
	}
}

// TestSKAEmissionRuleErrors ensures a block that violates an SKA emission rule
// is marked invalid so a competing block at the same height becomes the tip,
// while a block that only conflicts with the recorded emission state is
// rejected without being marked invalid.
func TestSKAEmissionRuleErrors(t *testing.T) {
	t.Run("OutsideWindow", func(t *testing.T) {
		g, emission := newSKAEmissionRuleHarness(t, 10, 100)
		parent := g.TipName()

		//   ... -> bsvh -> bbad (emission before the window opens)
		//              \-> bgood
		height := int64(g.Tip().Header.Height) + 1
		outs := g.OldestCoinbaseOuts()
		g.NextBlock("bbad", nil, outs[1:], func(b *wire.MsgBlock) {
			b.AddTransaction(emission(height, 1))
		})
		g.RejectTipBlock(ErrInvalidSKAEmission)
		expectKnownInvalid(g, "bbad", true)

		g.SetTip(parent)
		g.NextBlock("bgood", nil, outs[1:])
		g.AcceptTipBlock()
		g.ExpectTip("bgood")
	})

	t.Run("ForgedWithStateConflict", func(t *testing.T) {
		g, emission := newSKAEmissionRuleHarness(t, 1, 100)
		parent := g.TipName()

		//   ... -> bsvh -> bforged (emission with an unexpected nonce whose
		//                  \        output was changed after signing)
		//                   \-> bgood
		height := int64(g.Tip().Header.Height) + 1
		forged := emission(height, 7)
		forged.TxOut[0].SKAValue = new(big.Int).Sub(forged.TxOut[0].SKAValue,
			big.NewInt(1))
		outs := g.OldestCoinbaseOuts()
		g.NextBlock("bforged", nil, outs[1:], func(b *wire.MsgBlock) {
			b.AddTransaction(forged)
		})
		g.RejectTipBlock(ErrInvalidSKAEmission)
		expectKnownInvalid(g, "bforged", true)

		g.SetTip(parent)
		g.NextBlock("bgood", nil, outs[1:])
		g.AcceptTipBlock()
		g.ExpectTip("bgood")
	})

	t.Run("AlreadyEmitted", func(t *testing.T) {
		g, emission := newSKAEmissionRuleHarness(t, 1, 100)

		//   ... -> bsvh -> bemit -> bagain (emits SKA1 a second time with the
		//                                   next nonce, which conflicts with
		//                                   the recorded emitted flag)
		height := int64(g.Tip().Header.Height) + 1
		outs := g.OldestCoinbaseOuts()
		g.NextBlock("bemit", nil, outs[1:], func(b *wire.MsgBlock) {
			b.AddTransaction(emission(height, 1))
		})
		g.SaveTipCoinbaseOuts()
		g.AcceptTipBlock()

		outs = g.OldestCoinbaseOuts()
		g.NextBlock("bagain", nil, outs[1:], func(b *wire.MsgBlock) {
			b.AddTransaction(emission(height+1, 2))
		})
		_, err := g.chain.ProcessBlock(dcrutil.NewBlock(g.Tip()))
		if err == nil {
			t.Fatal("second SKA1 emission was accepted")
		}
		var rErr RuleError
		if errors.As(err, &rErr) || !isSKAEmissionStateError(err) ||
			!strings.Contains(err.Error(), "already been emitted") {
			t.Fatalf("second SKA1 emission not rejected as already emitted "+
				"without a rule error: %v", err)
		}
		expectKnownInvalid(g, "bagain", false)

		// A conflict with the recorded emission state must not hide a rule
		// violation later in the same block.
		//
		//   ... -> bsvh -> bemit -> bdouble (a second SKA1 emission followed
		//                                    by an emission whose expiry was
		//                                    changed after signing)
		badSigTx := emission(height+1, 2)
		badSigTx.Expiry--
		g.SetTip("bemit")
		g.NextBlock("bdouble", nil, outs[1:], func(b *wire.MsgBlock) {
			b.AddTransaction(emission(height+1, 2))
			b.AddTransaction(badSigTx)
		})
		g.RejectTipBlock(ErrInvalidSKAEmission)
		expectKnownInvalid(g, "bdouble", true)
	})

	t.Run("ReplayAfterWindow", func(t *testing.T) {
		const window = 2
		g, emission := newSKAEmissionRuleHarness(t, 1, window)

		//   ... -> bsvh -> bemit -> bw0 -> ... -> bw2 -> breplay
		//                                              \-> bnoexpiry
		//                                              \-> bgood
		//
		// breplay repeats the emission of bemit after the window has closed and
		// bnoexpiry repeats it without an expiry.
		height := int64(g.Tip().Header.Height) + 1
		emitTx := emission(height, 1)
		outs := g.OldestCoinbaseOuts()
		g.NextBlock("bemit", nil, outs[1:], func(b *wire.MsgBlock) {
			b.AddTransaction(emitTx)
		})
		g.SaveTipCoinbaseOuts()
		g.AcceptTipBlock()
		for i := 0; i <= window; i++ {
			outs := g.OldestCoinbaseOuts()
			g.NextBlock(fmt.Sprintf("bw%d", i), nil, outs[1:])
			g.SaveTipCoinbaseOuts()
			g.AcceptTipBlock()
		}
		parent := g.TipName()

		outs = g.OldestCoinbaseOuts()
		g.NextBlock("breplay", nil, outs[1:], func(b *wire.MsgBlock) {
			b.AddTransaction(emitTx)
		})
		g.RejectTipBlock(ErrExpiredTx)
		expectKnownInvalid(g, "breplay", true)

		g.SetTip(parent)
		noExpiryTx := emitTx.Copy()
		noExpiryTx.Expiry = wire.NoExpiryValue
		g.NextBlock("bnoexpiry", nil, outs[1:], func(b *wire.MsgBlock) {
			b.AddTransaction(noExpiryTx)
		})
		g.RejectTipBlock(ErrInvalidSKAEmission)
		expectKnownInvalid(g, "bnoexpiry", true)

		g.SetTip(parent)
		g.NextBlock("bgood", nil, outs[1:])
		g.AcceptTipBlock()
		g.ExpectTip("bgood")
	})
}
