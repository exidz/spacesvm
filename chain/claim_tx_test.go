// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package chain

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ava-labs/avalanchego/database/memdb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/crypto"
)

func TestClaimTx(t *testing.T) {
	t.Parallel()

	priv, err := f.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	sender, err := FormatPK(priv.PublicKey())
	if err != nil {
		t.Fatal(err)
	}

	priv2, err := f.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	sender2, err := FormatPK(priv2.PublicKey())
	if err != nil {
		t.Fatal(err)
	}

	db := memdb.New()
	defer db.Close()

	g := DefaultGenesis()
	ExpiryTime := int64(g.ExpiryTime)
	tt := []struct {
		tx        *ClaimTx
		blockTime int64
		err       error
	}{
		{ // invalid claim, [32]byte prefix is reserved for pubkey
			tx:        &ClaimTx{BaseTx: &BaseTx{Sender: sender, Prefix: bytes.Repeat([]byte{'a'}, crypto.SECP256K1RPKLen)}},
			blockTime: 1,
			err:       ErrPublicKeyMismatch,
		},
		{ // successful claim with expiry time "blockTime" + "expiryTime"
			tx:        &ClaimTx{BaseTx: &BaseTx{Sender: sender, Prefix: []byte("foo")}},
			blockTime: 1,
			err:       nil,
		},
		{ // invalid claim due to expiration
			tx:        &ClaimTx{BaseTx: &BaseTx{Sender: sender, Prefix: []byte("foo")}},
			blockTime: 100,
			err:       ErrPrefixNotExpired,
		},
		{ // successful new claim
			tx:        &ClaimTx{BaseTx: &BaseTx{Sender: sender, Prefix: []byte("foo")}},
			blockTime: ExpiryTime * 2,
			err:       nil,
		},
		{ // successful new claim by different owner
			tx:        &ClaimTx{BaseTx: &BaseTx{Sender: sender2, Prefix: []byte("foo")}},
			blockTime: ExpiryTime * 4,
			err:       nil,
		},
		{ // invalid claim due to expiration by different owner
			tx:        &ClaimTx{BaseTx: &BaseTx{Sender: sender2, Prefix: []byte("foo")}},
			blockTime: ExpiryTime*4 + 3,
			err:       ErrPrefixNotExpired,
		},
	}
	for i, tv := range tt {
		if i > 0 {
			// Expire old prefixes between txs
			if err := ExpireNext(db, tt[i-1].blockTime, tv.blockTime, true); err != nil {
				t.Fatalf("#%d: ExpireNext errored %v", i, err)
			}
		}
		err := tv.tx.Execute(g, db, uint64(tv.blockTime), ids.ID{})
		if !errors.Is(err, tv.err) {
			t.Fatalf("#%d: tx.Execute err expected %v, got %v", i, tv.err, err)
		}
		if tv.err != nil {
			continue
		}
		info, exists, err := GetPrefixInfo(db, tv.tx.Prefix)
		if err != nil {
			t.Fatalf("#%d: failed to get prefix info %v", i, err)
		}
		if !exists {
			t.Fatalf("#%d: failed to find prefix info", i)
		}
		if !bytes.Equal(info.Owner[:], tv.tx.Sender[:]) {
			t.Fatalf("#%d: unexpected owner found (expected pub key %q)", i, string(sender[:]))
		}
	}

	// Cleanup DB after all txs submitted
	if err := ExpireNext(db, 0, ExpiryTime*10, true); err != nil {
		t.Fatal(err)
	}
	pruned, err := PruneNext(db, 100)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 3 {
		t.Fatalf("expected to prune 3 but got %d", pruned)
	}
	_, exists, err := GetPrefixInfo(db, []byte("foo"))
	if err != nil {
		t.Fatalf("failed to get prefix info %v", err)
	}
	if exists {
		t.Fatal("prefix should not exist")
	}
}
