// Copyright 2020 The go-ethereum Authors
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

package eth

import (
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/fetcher"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// TxQueueSize is the size of the transaction queue used to enqueue transactions
var (
	TxQueueSize = runtime.NumCPU()
)

// enqueueTx is a channel to enqueue transactions in parallel.
// It is used to improve the performance of transaction enqueued.
var enqueueTx = make(chan func(), TxQueueSize)
var parallelCounter int32 = 0
var parallelCounterGuage = metrics.NewRegisteredGauge("p2p/enqueue/parallel", nil)
var txPackSizeGuage = metrics.NewRegisteredGauge("p2p/enqueue/tx/pack/size", nil)

func init() {
	log.Info("P2P euqneue parallel thread number", "threadNum", TxQueueSize)
	// run the transaction enqueuing loop
	for i := 0; i < TxQueueSize; i++ {
		go func() {
			for enqueue := range enqueueTx {
				atomic.AddInt32(&parallelCounter, 1)
				enqueue()
				atomic.AddInt32(&parallelCounter, -1)
			}
		}()
	}
	if metrics.EnabledExpensive {
		go func() {
			for {
				time.Sleep(1 * time.Second)
				parallelCounterGuage.Update(int64(atomic.LoadInt32(&parallelCounter)))
			}
		}()
	}
}

// ethHandler implements the eth.Backend interface to handle the various network
// packets that are sent as replies or broadcasts.
type ethHandler handler

func (h *ethHandler) Chain() *core.BlockChain { return h.chain }

// NilPool satisfies the TxPool interface but does not return any tx in the
// pool. It is used to disable transaction gossip.
type NilPool struct{}

// NilPool Get always returns nil
func (n NilPool) Get(hash common.Hash) *types.Transaction { return nil }

func (h *ethHandler) TxPool() eth.TxPool {
	if h.noTxGossip {
		return &NilPool{}
	}
	return h.txpool
}

// RunPeer is invoked when a peer joins on the `eth` protocol.
func (h *ethHandler) RunPeer(peer *eth.Peer, hand eth.Handler) error {
	return (*handler)(h).runEthPeer(peer, hand)
}

// PeerInfo retrieves all known `eth` information about a peer.
func (h *ethHandler) PeerInfo(id enode.ID) interface{} {
	if p := h.peers.peer(id.String()); p != nil {
		return p.info()
	}
	return nil
}

// AcceptTxs retrieves whether transaction processing is enabled on the node
// or if inbound transactions should simply be dropped.
func (h *ethHandler) AcceptTxs() bool {
	if h.noTxGossip {
		return false
	}
	return h.synced.Load()
}

// Handle is invoked from a peer's message handler when it receives a new remote
// message that the handler couldn't consume and serve itself.
func (h *ethHandler) Handle(peer *eth.Peer, packet eth.Packet) error {
	// Consume any broadcasts and announces, forwarding the rest to the downloader
	switch packet := packet.(type) {
	case *eth.NewBlockHashesPacket:
		hashes, numbers := packet.Unpack()
		return h.handleBlockAnnounces(peer, hashes, numbers)

	case *eth.NewBlockPacket:
		return h.handleBlockBroadcast(peer, packet.Block, packet.TD)

	case *eth.NewPooledTransactionHashesPacket:
		return h.txFetcher.Notify(peer.ID(), packet.Types, packet.Sizes, packet.Hashes)

	case *eth.TransactionsPacket:
		for _, tx := range *packet {
			if tx.Type() == types.BlobTxType {
				return errors.New("disallowed broadcast blob transaction")
			}
		}
		return asyncEnqueueTx(peer, *packet, h.txFetcher, false)

	case *eth.PooledTransactionsResponse:
		return asyncEnqueueTx(peer, *packet, h.txFetcher, true)

	default:
		return fmt.Errorf("unexpected eth packet type: %T", packet)
	}
}

func asyncEnqueueTx(peer *eth.Peer, txs []*types.Transaction, fetcher *fetcher.TxFetcher, directed bool) error {
	if working, err := fetcher.IsWorking(); !working {
		return err
	}
	//split the txs into multiple parts and enqueue them in parallel
	if metrics.EnabledExpensive {
		txPackSizeGuage.Update(int64(len(txs)))
	}
	for _, chunk := range splitTxs(txs, TxQueueSize) {
		// to avoid the closure to capture the same variable
		temp := chunk
		enqueueTx <- func() {
			if err := fetcher.Enqueue(peer.ID(), temp, directed); err != nil {
				peer.Log().Warn("Failed to enqueue transaction", "err", err)
			}
		}
	}
	return nil
}

func splitTxs(txs []*types.Transaction, chunks int) [][]*types.Transaction {
	var res [][]*types.Transaction
	var num int = 0
	if len(txs)%chunks == 0 {
		num = len(txs) / chunks
	} else {
		num = (len(txs) / chunks) + 1
	}
	if num == 0 {
		num = 1
	}
	for i := 0; i < len(txs); i += num {
		end := i + num
		if end > len(txs) {
			end = len(txs)
		}
		res = append(res, txs[i:end])
	}
	return res
}

// handleBlockAnnounces is invoked from a peer's message handler when it transmits a
// batch of block announcements for the local node to process.
func (h *ethHandler) handleBlockAnnounces(peer *eth.Peer, hashes []common.Hash, numbers []uint64) error {
	// Drop all incoming block announces from the p2p network if
	// the chain already entered the pos stage and disconnect the
	// remote peer.
	if h.merger.PoSFinalized() {
		return errors.New("disallowed block announcement")
	}
	// Schedule all the unknown hashes for retrieval
	var (
		unknownHashes  = make([]common.Hash, 0, len(hashes))
		unknownNumbers = make([]uint64, 0, len(numbers))
	)
	for i := 0; i < len(hashes); i++ {
		if !h.chain.HasBlock(hashes[i], numbers[i]) {
			unknownHashes = append(unknownHashes, hashes[i])
			unknownNumbers = append(unknownNumbers, numbers[i])
		}
	}
	for i := 0; i < len(unknownHashes); i++ {
		h.blockFetcher.Notify(peer.ID(), unknownHashes[i], unknownNumbers[i], time.Now(), peer.RequestOneHeader, peer.RequestBodies)
	}
	return nil
}

// handleBlockBroadcast is invoked from a peer's message handler when it transmits a
// block broadcast for the local node to process.
func (h *ethHandler) handleBlockBroadcast(peer *eth.Peer, block *types.Block, td *big.Int) error {
	// Drop all incoming block announces from the p2p network if
	// the chain already entered the pos stage and disconnect the
	// remote peer.
	if h.merger.PoSFinalized() {
		return errors.New("disallowed block broadcast")
	}
	// Schedule the block for import
	h.blockFetcher.Enqueue(peer.ID(), block)

	// Assuming the block is importable by the peer, but possibly not yet done so,
	// calculate the head hash and TD that the peer truly must have.
	var (
		trueHead = block.ParentHash()
		trueTD   = new(big.Int).Sub(td, block.Difficulty())
	)
	// Update the peer's total difficulty if better than the previous
	if _, td := peer.Head(); trueTD.Cmp(td) > 0 {
		peer.SetHead(trueHead, trueTD)
		h.chainSync.handlePeerEvent()
	}
	return nil
}
