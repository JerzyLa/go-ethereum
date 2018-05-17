// Copyright 2016 The go-ethereum Authors
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

package storage

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/log"
	lru "github.com/hashicorp/golang-lru"
)

// NetStore
type NetStore struct {
	mu       sync.Mutex
	store    ChunkStore
	fetchers *lru.Cache
	retrieve func(Request, *Fetcher) (context.Context, error)
}

func NewNetStore(store ChunkStore, retrieve func(Request, *Fetcher) (context.Context, error)) (*NetStore, error) {
	fetchers, err := lru.New(defaultChunkRequestsCacheCapacity)
	if err != nil {
		return nil, err
	}
	return &NetStore{
		store:    store,
		fetchers: fetchers,
		retrieve: retrieve,
	}, nil
}

// Put stores a chunk in localstore, returns a wait function to wait for
// storage unless it is found
func (n *NetStore) Put(ch Chunk) (func(ctx context.Context) error, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	wait, err := n.store.Put(ch)
	if err != nil {
		return nil, err
	}
	// if chunk was already in store (wait f is nil)
	if wait == nil {
		return nil, nil
	}
	// if chunk is now put in store, check if there was an active fetcher
	f, _ := n.fetchers.Get(ch.Address())
	// if there is, deliver the chunk to requestors via fetcher
	if f != nil {
		f.(*Fetcher).deliver(ch)
	}
	return wait, nil
}

// Has checks if chunk with hash address ref is stored locally
// if not it returns a fetcher function to be called with a context
// block until item is stored
func (n *NetStore) Has(ref Address) (func(Request) error, error) {
	chunk, fetch, err := n.get(ref)
	if chunk != nil {
		return nil, nil
	}
	wait := func(rctx Request) error {
		_, err = fetch(rctx)
		// TODO: exact logic for waiting till stored
		return err
	}
	return wait, nil
}

// get attempts at retrieving the chunk from LocalStore
// if it is not found, attempts at retrieving an existing Fetchers
// if none exists, creates one and saves it in the Fetchers cache
// From here on, all Get will hit on this Fetcher until the chunk is delivered
// or all Fetcher contexts are done
// it returns a chunk, a fetcher function and an error
// if chunk is nil, fetcher needs to be called with a context to return the chunk
func (n *NetStore) get(ref Address) (Chunk, func(Request) (Chunk, error), error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	chunk, err := n.store.Get(ref)
	if err == nil {
		return chunk, nil, nil
	}
	f, err := n.getOrCreateFetcher(ref)
	if err != nil {
		return nil, nil, err
	}
	return nil, f.fetch, nil
}

// getOrCreateFetcher attempts at retrieving an existing Fetchers
// if none exists, creates one and saves it in the Fetchers cache
// caller must hold the lock
func (n *NetStore) getOrCreateFetcher(ref Address) (*Fetcher, error) {
	key := hex.EncodeToString(ref)
	log.Debug("getOrCreateFetcher", "fetchers.Len", n.fetchers.Len())
	fetcher, ok := n.fetchers.Get(key)
	if ok {
		log.Debug("getOrCreateFetcher found fetcher")
		return fetcher.(*Fetcher), nil
	}
	log.Debug("getOrCreateFetcher new fetcher")
	f := NewFetcher(ref, n.retrieve, func() {
		log.Debug("removing fetcher")
		log.Debug("fetchers before remove", "fetchers.Len", n.fetchers.Len())
		n.fetchers.Remove(key)
		log.Debug("fetchers after remove", "fetchers.Len", n.fetchers.Len())
	})
	n.fetchers.Add(key, f)

	return f, nil
}

// Get retrieves the chunk from the NetStore DPA synchronously
// it calls NetStore get. If the chunk is not in local Storage
// it calls fetch with the request, which blocks until the chunk
// arrived or context is done
func (n *NetStore) Get(rctx Request, ref Address) (Chunk, error) {
	chunk, fetch, err := n.get(ref)
	if err != nil {
		return nil, err
	}
	if chunk != nil {
		return chunk, nil
	}
	return fetch(rctx)
}

// // waitAndRemoveFetcher waits till all actual Requests are closed, removes the Fetcher from fetchers
// // and stops the fetcher.
// func (n *NetStore) waitAndRemoveFetcher(f *Fetcher) {
// 	log.Debug("waitAndRemoveFetcher started")
// 	f.wait()
// 	log.Debug("waitAndRemoveFetcher after wait")
// 	log.Warn("remove fetcher")
// 	n.fetchers.Remove(hex.EncodeToString(f.addr))
// 	f.stop()
// }

// Close chunk store
func (n *NetStore) Close() {
	n.store.Close()
}

// Request is an extention of context.Context and is handed by client to the fetcher
type Request interface {
	context.Context
	Address() Address
}

type errStatus struct {
	error
	status error
}

func (e *errStatus) Status() error {
	return e.status
}

// Fetcher is created when a chunk is not found locally
// it starts a request handler loop once and keeps it
// alive until all active requests complete
// either because the chunk is delivered or requestor cancelled/timed out
// fetcher self destroys
// TODO: cancel all forward requests after termination
type Fetcher struct {
	*chunk     // the delivered chunk
	retrieve   func(Request, *Fetcher) (context.Context, error)
	remove     func()
	addr       Address      //
	requestC   chan Request // incoming requests
	requestCnt int32
	deliveredC chan struct{} // chan signalling chunk delivery to requests
	stoppedC   chan struct{} // closed when terminates
	// requestsWg sync.WaitGroup // wait group on requests
	status error // fetching status
}

// NewFetcher creates a new fetcher for a chunk
// stored in netstore's fetchers (LRU cache) keyed by address
func NewFetcher(addr Address, retrieve func(Request, *Fetcher) (context.Context, error), remove func()) *Fetcher {
	f := &Fetcher{
		addr:       addr,
		retrieve:   retrieve,
		remove:     remove,
		requestC:   make(chan Request),
		deliveredC: make(chan struct{}),
		stoppedC:   make(chan struct{}),
	}
	go f.start()
	return f
}

// Deliver sets the chunk on the Fetcher and closes the deliveredC channel
// to signal to individual Fetchers the arrival
func (f *Fetcher) deliver(ch Chunk) {
	f.chunk = ch.Chunk()
	close(f.deliveredC)
}

// fetch is a synchronous fetcher function returned
// by NetStore.Get and called if remote fetching is required
func (f *Fetcher) fetch(rctx Request) (Chunk, error) {
	atomic.AddInt32(&f.requestCnt, 1)
	log.Debug("fetch", "requestCnt", f.requestCnt)
	defer func() {
		log.Debug("fetch defer", "requestCnt", f.requestCnt)
		if atomic.AddInt32(&f.requestCnt, -1) == 0 {
			f.remove()
			close(f.stoppedC)
		}
	}()

	// put rctx on request channel
	select {
	case f.requestC <- rctx:
	case <-f.stoppedC:
	}

	select {
	case <-rctx.Done():
		log.Warn("context done")
		return nil, &errStatus{rctx.Err(), f.status}
	case <-f.deliveredC:
		return f.chunk, nil
	}
}

// // wait waits till all actual Requests are closed
// func (f *Fetcher) wait() {
// 	<-f.quitC
// 	// remove the Fetcher from the cache when all Fetchers
// 	// contexts closed, self-destruct and remove from fetchers
// }

// // stop stops the Fetcher
// func (f *Fetcher) stop() {
// 	close(f.stoppedC)
// }

// start prepares the Fetcher
// it keeps the Fetcher alive
func (f *Fetcher) start() {
	var (
		doRetrieve bool               // determines if retrieval is initiated in the current iteration
		forwardC   = make(chan error) // timeout/error on one of the forwards
		rctx       Request
	)
F:
	// loop that keeps the Fetcher alive
	for {
		// if forwarding is wanted, call netStore
		if doRetrieve {
			go func() {
				ctx, err := f.retrieve(rctx, f)
				if err != nil {
					select {
					case forwardC <- err:
						log.Warn("forward result", "err", err)
					case <-f.stoppedC:
					}
				} else {
					go func() {
						timer := time.NewTimer(searchTimeout)
						var err error
						select {
						case <-ctx.Done():
							err = ctx.Err()
						case <-timer.C:
							err = errors.New("search timed out")
						case <-f.stoppedC:
							return
						}
						select {
						case forwardC <- err:
						case <-f.stoppedC:
						}
					}()
				}
			}()
			doRetrieve = false
		}

		select {

		// ready to receive by accept in a request ()
		case rctx = <-f.requestC:
			log.Warn("upstream request received")
			doRetrieve = true

		// rerequest upon forwardC event
		case err := <-forwardC: // if forward request completes
			log.Warn("forward request failed", "err", err)
			f.status = err
			doRetrieve = err != nil

		case <-f.stoppedC:
			log.Warn("quitmain loop")
			// all Fetcher context closed, can quit
			break F
		}
	}

}
