// (c) 2020-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package evm

import (
	"fmt"

	"github.com/ava-labs/avalanchego/chains/atomic"
	"github.com/ava-labs/avalanchego/codec"
	"github.com/ava-labs/avalanchego/database/versiondb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/coreth/core"
	"github.com/ava-labs/coreth/ethdb"
	"github.com/ava-labs/coreth/trie"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

const atomicTrieTipBufferSize = 1

// AtomicBackend abstracts the verification and processing
// of atomic transactions
type AtomicBackend interface {
	InsertTxs(blockHash common.Hash, blockHeight uint64, parentHash common.Hash, txs []*Tx) (AtomicState, error)
	CalculateRootWithTxs(blockHeight uint64, parentHash common.Hash, txs []*Tx) (common.Hash, error)

	// ResetLastAccepted is used after state-sync to reload the initial
	// accepted block.
	ResetLastAccepted(hash common.Hash)
	// GetLastAccepted returns the last accepted root
	GetLastAccepted() common.Hash
}

// AtomicState is an abstraction created through AtomicBackend
// and can be used to apply the VM's state change for atomic txs
// or reject them to free memory.
// The root of the atomic trie after applying the state change
// is accessible through this interface as well.
type AtomicState interface {
	Root() common.Hash
	Accept() error
	Reject() error
}

type atomicBackend struct {
	trieDB         *trie.Database
	tipBuffer      *core.BoundedBuffer
	commitInterval uint64
	memoryCap      common.StorageSize

	lastAcceptedRoot common.Hash
	lastAcceptedHash common.Hash
	verifiedRoots    map[common.Hash]common.Hash

	bonusBlocks  map[uint64]ids.ID   // Map of height to blockID for blocks to skip indexing
	db           *versiondb.Database // Underlying database
	sharedMemory atomic.SharedMemory

	codec codec.Manager

	repo       AtomicTxRepository
	atomicTrie AtomicTrie
}

// TODO: this code pulls dependencies from the AtomicTrie, but this
// may not be the ideal direction going forward.
func NewAtomicBackend(
	codec codec.Manager,
	db *versiondb.Database, sharedMemory atomic.SharedMemory,
	bonusBlocks map[uint64]ids.ID, repo AtomicTxRepository, atomicTrie AtomicTrie,
	commitHeightInterval uint64, lastAcceptedHash common.Hash,
) (AtomicBackend, error) {

	trieDB := atomicTrie.TrieDB()
	return &atomicBackend{
		db:               db,
		sharedMemory:     sharedMemory,
		bonusBlocks:      bonusBlocks,
		repo:             repo,
		atomicTrie:       atomicTrie,
		codec:            codec,
		trieDB:           trieDB,
		lastAcceptedHash: lastAcceptedHash,
		lastAcceptedRoot: atomicTrie.GetInitializedRoot(),
		tipBuffer:        core.NewBoundedBuffer(atomicTrieTipBufferSize, trieDB.Dereference),
		verifiedRoots:    make(map[common.Hash]common.Hash),
		commitInterval:   commitHeightInterval,
	}, nil
}

func (a *atomicBackend) getAtomicRootAt(blockHash common.Hash) (common.Hash, error) {
	// TODO: we can implement this in a few ways.
	if blockHash == a.lastAcceptedHash {
		return a.lastAcceptedRoot, nil
	}
	if root, ok := a.verifiedRoots[blockHash]; ok {
		return root, nil
	}
	return common.Hash{}, fmt.Errorf("attempt to access atomic root for an invalid block: %s", blockHash)
}

// ResetLastAccepted is used after state-sync to reload the initial
// accepted block.
// The last accepted atomic root is synced from the atomicTrie.
// TODO: try to remove this
func (a *atomicBackend) ResetLastAccepted(blockHash common.Hash) {
	a.lastAcceptedHash = blockHash
	a.lastAcceptedRoot = a.atomicTrie.GetInitializedRoot()
}

func (a *atomicBackend) GetLastAccepted() common.Hash {
	return a.lastAcceptedRoot
}

func (a *atomicBackend) CalculateRootWithTxs(blockHeight uint64, parentHash common.Hash, txs []*Tx) (common.Hash, error) {
	parentRoot, err := a.getAtomicRootAt(parentHash)
	if err != nil {
		return common.Hash{}, err
	}
	tr, err := a.atomicTrie.OpenTrie(parentRoot)
	if err != nil {
		return common.Hash{}, err
	}
	atomicOps, err := mergeAtomicOps(txs)
	if err != nil {
		return common.Hash{}, err
	}
	if err := tr.UpdateTrie(blockHeight, atomicOps); err != nil {
		return common.Hash{}, err
	}

	return tr.Root(), nil
}

func (a *atomicBackend) InsertTxs(blockHash common.Hash, blockHeight uint64, parentHash common.Hash, txs []*Tx) (AtomicState, error) {
	parentRoot, err := a.getAtomicRootAt(parentHash)
	if err != nil {
		return nil, err
	}
	tr, err := a.atomicTrie.OpenTrie(parentRoot)
	if err != nil {
		return nil, err
	}
	atomicOps, err := mergeAtomicOps(txs)
	if err != nil {
		return nil, err
	}
	if err := tr.UpdateTrie(blockHeight, atomicOps); err != nil {
		return nil, err
	}

	// pin the atomic trie changes to memory and get the new root.
	root, err := tr.Commit()
	if err != nil {
		return nil, err
	}

	if err := a.insertTrie(root); err != nil {
		return nil, err
	}
	a.verifiedRoots[blockHash] = root

	return &atomicState{
		backend:     a,
		blockHash:   blockHash,
		blockHeight: blockHeight,
		txs:         txs,
		atomicOps:   atomicOps,
		atomicRoot:  root,
	}, nil
}

func (a *atomicBackend) insertTrie(root common.Hash) error {
	a.trieDB.Reference(root, common.Hash{})

	// The use of [Cap] in [insertTrie] prevents exceeding the configured memory
	// limit (and OOM) in case there is a large backlog of processing (unaccepted) blocks.
	nodes, _ := a.trieDB.Size()
	if nodes <= a.memoryCap {
		return nil
	}
	if err := a.trieDB.Cap(a.memoryCap - ethdb.IdealBatchSize); err != nil {
		return fmt.Errorf("failed to cap atomic trie for root %s: %w", root, err)
	}

	return nil
}

func (a *atomicBackend) acceptTrie(height uint64, root common.Hash) error {
	// Attempt to dereference roots at least [tipBufferSize] old
	//
	// Note: It is safe to dereference roots that have been committed to disk
	// (they are no-ops).
	a.tipBuffer.Insert(root)

	// Commit this root if we have reached the [commitInterval].
	modCommitInterval := height % a.commitInterval
	if modCommitInterval == 0 {
		if err := a.atomicTrie.UpdateLastCommitted(root, height); err != nil {
			return err
		}
		if err := a.trieDB.Commit(root, false, nil); err != nil {
			return fmt.Errorf("failed to commit atomict trie for root %s at height %d: %w", root, height, err)
		}
		log.Info("committed atomic trie", "root", root.String(), "height", height)
	}
	return nil
}

type atomicState struct {
	backend     *atomicBackend
	blockHash   common.Hash
	blockHeight uint64
	txs         []*Tx
	atomicOps   map[ids.ID]*atomic.Requests
	atomicRoot  common.Hash
}

func (a *atomicState) isBonus() bool {
	if bonusID, found := a.backend.bonusBlocks[a.blockHeight]; found {
		return bonusID == ids.ID(a.blockHash)
	}
	return false
}

func (a *atomicState) Accept() error {
	// Update the atomic tx repository. Note it is necessary to invoke
	// the correct method taking bonus blocks into consideration.
	if a.isBonus() {
		if err := a.backend.repo.WriteBonus(a.blockHeight, a.txs); err != nil {
			return err
		}
	} else {
		if err := a.backend.repo.Write(a.blockHeight, a.txs); err != nil {
			return err
		}
	}

	if err := a.backend.acceptTrie(a.blockHeight, a.atomicRoot); err != nil {
		return err
	}
	a.backend.lastAcceptedHash = a.blockHash
	a.backend.lastAcceptedRoot = a.atomicRoot
	delete(a.backend.verifiedRoots, a.blockHash)

	// If this is a bonus block, commit the database without applying atomic ops
	// to shared memory.
	if a.isBonus() {
		log.Info("skipping atomic tx acceptance on bonus block", "block", a.blockHash)
		return a.backend.db.Commit()
	}

	// Otherwise, atomically commit pending changes in the version db with
	// atomic ops to shared memory.
	batch, err := a.backend.db.CommitBatch()
	if err != nil {
		return fmt.Errorf("failed to create commit batch due to: %w", err)
	}
	return a.backend.sharedMemory.Apply(a.atomicOps, batch)
}

func (a *atomicState) Reject() error {
	a.backend.trieDB.Dereference(a.atomicRoot)
	delete(a.backend.verifiedRoots, a.blockHash)
	return nil
}

func (a *atomicState) Root() common.Hash {
	return a.atomicRoot
}
