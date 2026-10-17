// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package indexers

import (
	"bytes"
	"math/big"
	"testing"
	"time"

	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/database"
	"github.com/monetarium/monetarium-node/wire"
)

// flushCheckChain reports whether a database transaction is still open when
// LookupUTXO queries the chain. db.Flush waits for every open transaction, the
// same way block processing does while it holds the chain lock.
type flushCheckChain struct {
	*testChain
	t        *testing.T
	db       database.DB
	unspent  wire.OutPoint
	blockedN int
}

func (c *flushCheckChain) FetchUtxoEntrySKADetails(op wire.OutPoint) (*big.Int, int64, uint32, bool, error) {
	done := make(chan error, 1)
	go func() { done <- c.db.Flush() }()
	select {
	case err := <-done:
		if err != nil {
			c.t.Errorf("flush: %v", err)
		}
	case <-time.After(time.Second):
		c.blockedN++
	}
	if op == c.unspent {
		return big.NewInt(5), 42, 3, false, nil
	}
	return nil, 0, 0, true, nil
}

// TestSSFeeLookupUTXOOutsideDBTx ensures LookupUTXO closes its database
// transaction before taking the chain lock, so a UTXO cache flush during block
// processing cannot deadlock with block template generation.
func TestSSFeeLookupUTXOOutsideDBTx(t *testing.T) {
	db := setupDB(t)
	base, err := newTestChain()
	if err != nil {
		t.Fatal(err)
	}

	hash160 := bytes.Repeat([]byte{0x11}, 20)
	key, err := makeSSFeeIndexKey(ssfeeTypeMiner, cointype.CoinTypeVAR, hash160)
	if err != nil {
		t.Fatal(err)
	}
	ops := []wire.OutPoint{
		{Hash: chainhash.Hash{1}, Index: 0, Tree: wire.TxTreeRegular},
		{Hash: chainhash.Hash{2}, Index: 1, Tree: wire.TxTreeRegular},
	}
	err = db.Update(func(dbTx database.Tx) error {
		bucket, err := dbTx.Metadata().CreateBucketIfNotExists(ssfeeIndexKey)
		if err != nil {
			return err
		}
		return bucket.Put(key, serializeOutPoints(ops))
	})
	if err != nil {
		t.Fatal(err)
	}

	chain := &flushCheckChain{testChain: base, t: t, db: db, unspent: ops[1]}
	idx := &SSFeeIndex{db: db, chain: chain}

	op, value, height, index, err := idx.LookupUTXO(true, cointype.CoinTypeVAR, hash160)
	if err != nil {
		t.Fatal(err)
	}
	if chain.blockedN != 0 {
		t.Fatalf("chain queried %d time(s) while a database transaction was open", chain.blockedN)
	}
	if op == nil || *op != ops[1] || value.Cmp(big.NewInt(5)) != 0 || height != 42 || index != 3 {
		t.Fatalf("got op=%v value=%v height=%d index=%d, want %v 5 42 3", op, value, height, index, ops[1])
	}
}
