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
// signed with the given nonce after applying the given changes to the
// transaction and its authorization.  SKA2 shares the SKA1 configuration but
// has no activation vote.
func newSKAEmissionRuleHarness(t *testing.T, emitOffset, window int64) (*chaingenHarness,
	func(height int64, nonce uint64,
		mutate ...func(*wire.MsgTx, *chaincfg.SKAEmissionAuth)) *wire.MsgTx) {

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
	ska2 := *params.SKACoins[1]
	ska2.CoinType, ska2.Name, ska2.Symbol = 2, "Skarb-2", "SKA2"
	params.SKACoins[2] = &ska2
	params.InitialSKATypes = []cointype.CoinType{1}

	g := newChaingenHarness(t, params)
	g.AdvanceToStakeValidationHeight()

	emission := func(height int64, nonce uint64,
		mutate ...func(*wire.MsgTx, *chaincfg.SKAEmissionAuth)) *wire.MsgTx {

		tx := createTestEmissionTx(t, params.SKACoins[1].EmissionAddresses,
			[]*big.Int{amount}, 1, params)
		auth := &chaincfg.SKAEmissionAuth{
			EmissionKey: priv.PubKey(),
			CoinType:    1,
			Nonce:       nonce,
			Amount:      amount,
			Height:      height,
		}
		for _, f := range mutate {
			f(tx, auth)
		}
		signEmissionTx(t, tx, auth, priv, params)
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

// expectStateConflict ensures the named block is rejected with a conflict with
// the recorded SKA emission state whose message contains want, without a rule
// error, and is not marked invalid.
func expectStateConflict(g *chaingenHarness, blockName, want string) {
	g.t.Helper()

	_, err := g.chain.ProcessBlock(dcrutil.NewBlock(g.BlockByName(blockName)))
	var rErr RuleError
	if err == nil || errors.As(err, &rErr) || !isSKAEmissionStateError(err) ||
		!strings.Contains(err.Error(), want) {
		g.t.Fatalf("block %q not rejected with a %q state conflict without a "+
			"rule error: %v", blockName, want, err)
	}
	expectKnownInvalid(g, blockName, false)
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
		expectStateConflict(g, "bagain", "already been emitted")

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

	t.Run("RuleViolations", func(t *testing.T) {
		g, emission := newSKAEmissionRuleHarness(t, 1, 100)
		parent := g.TipName()
		height := int64(g.Tip().Header.Height) + 1
		outs := g.OldestCoinbaseOuts()

		// bauthver through bmulti each hold the valid SKA1 emission at this
		// height with one change that breaks a single rule, signed after the
		// change.  bnoncesig and bnonceska break a rule next to an emission
		// with an unexpected nonce, which must not hide the violation.
		//
		//   ... -> bsvh -> bauthver  (authorization encoded as version 2)
		//              \-> blocklock (lock time set)
		//              \-> bexpiry   (expiry past the window end)
		//              \-> bwrongkey (signed with another key)
		//              \-> baddr     (output pays another script hash)
		//              \-> bamount   (output and authorized amount one atom short)
		//              \-> bauthamt  (authorized amount one atom short)
		//              \-> bcointype (SKA2 authorization on SKA1 outputs)
		//              \-> bnokey    (authorization for unconfigured SKA3)
		//              \-> bvote     (SKA2 emission without its activation vote)
		//              \-> bmulti    (two SKA1 emissions)
		//              \-> bnoncesig (nonce 2 emission and a forged signature)
		//              \-> bnonceska (nonce 2 emission and a payment in SKA3)
		//              \-> bgood -> bnonce (nonce 2 emission before any SKA1
		//                                   emission)
		rejectSibling := func(blockName string, txns ...*wire.MsgTx) {
			t.Helper()

			g.SetTip(parent)
			g.NextBlock(blockName, nil, outs[1:], func(b *wire.MsgBlock) {
				for _, tx := range txns {
					b.AddTransaction(tx)
				}
			})
			g.RejectTipBlock(ErrInvalidSKAEmission)
			expectKnownInvalid(g, blockName, true)
		}

		// Byte 4 is the authorization version, after the 4-byte SKA marker.
		authVer := emission(height, 1)
		authVer.TxIn[0].SignatureScript[4] = 0x02
		rejectSibling("bauthver", authVer)

		rejectSibling("blocklock", emission(height, 1,
			func(tx *wire.MsgTx, _ *chaincfg.SKAEmissionAuth) {
				tx.LockTime = 1
			}))

		rejectSibling("bexpiry", emission(height, 1,
			func(tx *wire.MsgTx, _ *chaincfg.SKAEmissionAuth) {
				tx.Expiry++
			}))

		otherKey, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			t.Fatalf("failed to generate key: %v", err)
		}
		wrongKey := emission(height, 1)
		signEmissionTx(t, wrongKey, &chaincfg.SKAEmissionAuth{
			EmissionKey: otherKey.PubKey(),
			CoinType:    1,
			Nonce:       1,
			Amount:      wrongKey.TxOut[0].SKAValue,
			Height:      height,
		}, otherKey, g.chain.chainParams)
		rejectSibling("bwrongkey", wrongKey)

		rejectSibling("baddr", emission(height, 1,
			func(tx *wire.MsgTx, _ *chaincfg.SKAEmissionAuth) {
				tx.TxOut[0].PkScript[2] ^= 0xff
			}))

		rejectSibling("bamount", emission(height, 1,
			func(tx *wire.MsgTx, auth *chaincfg.SKAEmissionAuth) {
				auth.Amount = new(big.Int).Sub(auth.Amount, big.NewInt(1))
				tx.TxOut[0].SKAValue = auth.Amount
			}))

		rejectSibling("bauthamt", emission(height, 1,
			func(_ *wire.MsgTx, auth *chaincfg.SKAEmissionAuth) {
				auth.Amount = new(big.Int).Sub(auth.Amount, big.NewInt(1))
			}))

		rejectSibling("bcointype", emission(height, 1,
			func(_ *wire.MsgTx, auth *chaincfg.SKAEmissionAuth) {
				auth.CoinType = 2
			}))

		rejectSibling("bnokey", emission(height, 1,
			func(_ *wire.MsgTx, auth *chaincfg.SKAEmissionAuth) {
				auth.CoinType = 3
			}))

		rejectSibling("bvote", emission(height, 1,
			func(tx *wire.MsgTx, auth *chaincfg.SKAEmissionAuth) {
				tx.TxOut[0].CoinType = 2
				auth.CoinType = 2
			}))

		rejectSibling("bmulti", emission(height, 1), emission(height, 1,
			func(tx *wire.MsgTx, _ *chaincfg.SKAEmissionAuth) {
				tx.Expiry--
			}))

		badSigTx := emission(height, 1)
		badSigTx.Expiry--
		rejectSibling("bnoncesig", emission(height, 2), badSigTx)

		g.SetTip(parent)
		g.NextBlock("bnonceska", &outs[0], outs[1:], func(b *wire.MsgBlock) {
			out := b.Transactions[1].TxOut[0]
			out.CoinType, out.SKAValue, out.Value = 3, big.NewInt(out.Value), 0
			b.AddTransaction(emission(height, 2))
		})
		g.RejectTipBlock(ErrInvalidSKAEmission)
		expectKnownInvalid(g, "bnonceska", true)

		g.SetTip(parent)
		g.NextBlock("bgood", nil, outs[1:])
		g.SaveTipCoinbaseOuts()
		g.AcceptTipBlock()
		g.ExpectTip("bgood")

		// bnonce extends bgood rather than competing with it, since a block
		// that is not marked invalid wins a tie with a later sibling.
		outs = g.OldestCoinbaseOuts()
		g.NextBlock("bnonce", nil, outs[1:], func(b *wire.MsgBlock) {
			b.AddTransaction(emission(height+1, 2))
		})
		expectStateConflict(g, "bnonce", "invalid nonce")
	})
}
