// Copyright 2018 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package localstore

import (
	"bytes"
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/swarm/storage"
)

// TestAccessors tests most basic Put and Get functionalities
// for different accessors.
func TestAccessors(t *testing.T) {
	db, cleanupFunc := newTestDB(t)
	defer cleanupFunc()

	testAccessors(t, db)
}

// TestAccessors_withRetrievalCompositeIndex tests most basic
// Put and Get functionalities for different accessors
// by using retrieval composite index.
func TestAccessors_withRetrievalCompositeIndex(t *testing.T) {
	db, cleanupFunc := newTestDB(t, WithRetrievalCompositeIndex(true))
	defer cleanupFunc()

	testAccessors(t, db)
}

// testAccessors tests most basic Put and Get functionalities
// for different accessors. This test validates that the chunk
// is retrievable from the database, not if all indexes are set
// correctly.
func testAccessors(t *testing.T, db *DB) {
	for _, m := range []Mode{
		ModeSyncing,
		ModeUpload,
		ModeRequest,
	} {
		t.Run(ModeName(m), func(t *testing.T) {
			a := db.Accessor(m)

			want := generateRandomChunk()

			err := a.Put(context.Background(), want)
			if err != nil {
				t.Fatal(err)
			}

			got, err := a.Get(context.Background(), want.Address())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Data(), want.Data()) {
				t.Errorf("got chunk data %x, want %x", got.Data(), want.Data())
			}
		})
	}

	// Synced mode does not put the item to retrieval index.
	t.Run(ModeName(ModeSynced), func(t *testing.T) {
		a := db.Accessor(ModeSynced)

		chunk := generateRandomChunk()

		// first put a random chunk to the database
		err := a.Put(context.Background(), chunk)
		if err != nil {
			t.Fatal(err)
		}

		wantError := storage.ErrChunkNotFound
		_, err = a.Get(context.Background(), chunk.Address())
		if err != wantError {
			t.Errorf("got error %v, want %v", err, wantError)
		}
	})

	// Access mode is a special as it does not store the chunk
	// in the database.
	t.Run(ModeName(modeAccess), func(t *testing.T) {
		a := db.Accessor(ModeUpload)

		want := generateRandomChunk()

		// first put a random chunk to the database
		err := a.Put(context.Background(), want)
		if err != nil {
			t.Fatal(err)
		}

		a = db.Accessor(modeAccess)

		got, err := a.Get(context.Background(), want.Address())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Data(), want.Data()) {
			t.Errorf("got chunk data %x, want %x", got.Data(), want.Data())
		}
	})

	// Removal mode is a special case as it removes the chunk
	// from the database.
	t.Run(ModeName(modeRemoval), func(t *testing.T) {
		a := db.Accessor(ModeUpload)

		want := generateRandomChunk()

		// first put a random chunk to the database
		err := a.Put(context.Background(), want)
		if err != nil {
			t.Fatal(err)
		}

		got, err := a.Get(context.Background(), want.Address())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Data(), want.Data()) {
			t.Errorf("got chunk data %x, want %x", got.Data(), want.Data())
		}

		a = db.Accessor(modeRemoval)

		// removal accessor actually removes the chunk on Put
		err = a.Put(context.Background(), want)
		if err != nil {
			t.Fatal(err)
		}

		// chunk should not be found
		wantErr := storage.ErrChunkNotFound
		_, err = a.Get(context.Background(), want.Address())
		if err != wantErr {
			t.Errorf("got error %v, expected %v", err, wantErr)
		}
	})
}
