// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package vm implements custom VM.
package vm

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/database/manager"
	"github.com/ava-labs/avalanchego/database/versiondb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/choices"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	snowmanblock "github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/utils/json"
	"github.com/gorilla/rpc/v2"
	log "github.com/inconshreveable/log15"

	"github.com/ava-labs/quarkvm/chain"
	"github.com/ava-labs/quarkvm/mempool"
	"github.com/ava-labs/quarkvm/version"
)

const Name = "quarkvm"

var (
	_ snowmanblock.ChainVM = &VM{}
	_ chain.VM             = &VM{}
)

var (
	ErrNoPendingTx = errors.New("no pending tx")
)

// TODO: add separate chain state manager?
// TODO: add mutex?

type VM struct {
	ctx     *snow.Context
	db      database.Database
	mempool *mempool.Mempool

	l sync.Mutex

	// Block ID --> Block
	// Each element is a block that passed verification but
	// hasn't yet been accepted/rejected
	verifiedBlocks map[ids.ID]*chain.Block

	toEngine chan<- common.Message

	preferred    ids.ID
	lastAccepted ids.ID
}

// implements "snowmanblock.ChainVM.common.VM"
func (vm *VM) Initialize(
	ctx *snow.Context,
	dbManager manager.Manager,
	genesisBytes []byte,
	upgradeBytes []byte,
	configBytes []byte,
	toEngine chan<- common.Message,
	_ []*common.Fx,
	_ common.AppSender,
) error {
	log.Info("initializing quarkvm", "version", version.Version)

	vm.ctx = ctx
	vm.db = dbManager.Current().Database
	vm.mempool = mempool.New(1024)
	vm.verifiedBlocks = make(map[ids.ID]*chain.Block)
	vm.toEngine = toEngine

	// Try to load last accepted
	b, err := chain.GetLastAccepted(vm.db)
	if err != nil {
		log.Error("could not get last accepted", "err", err)
		return err
	}
	if b != (ids.ID{}) {
		vm.preferred = b
		vm.lastAccepted = b
		log.Info("initialized quarkvm from last accepted", "block", b)
		return nil
	}

	// Load from genesis
	genesisBlk, err := chain.InitializeBlock(
		genesisBytes,
		choices.Accepted,
		vm,
	)
	if err != nil {
		log.Error("unable to init genesis block", "err", err)
		return err
	}
	if err := chain.SetLastAccepted(vm.db, genesisBlk); err != nil {
		log.Error("could not set genesis as last accepted", "err", err)
		return err
	}
	gBlkID := genesisBlk.ID()
	vm.preferred = gBlkID
	vm.lastAccepted = gBlkID
	log.Info("initialized quarkvm from genesis", "block", gBlkID)
	return nil
}

// implements "snowmanblock.ChainVM.common.VM"
func (vm *VM) Bootstrapping() error {
	return nil
}

// implements "snowmanblock.ChainVM.common.VM"
func (vm *VM) Bootstrapped() error {
	return nil
}

// implements "snowmanblock.ChainVM.common.VM"
func (vm *VM) Shutdown() error {
	if vm.ctx == nil {
		return nil
	}
	return vm.db.Close()
}

// implements "snowmanblock.ChainVM.common.VM"
func (vm *VM) Version() (string, error) { return version.Version.String(), nil }

// implements "snowmanblock.ChainVM.common.VM"
// for "ext/vm/[chainID]"
func (vm *VM) CreateHandlers() (map[string]*common.HTTPHandler, error) {
	server := rpc.NewServer()
	server.RegisterCodec(json.NewCodec(), "application/json")
	server.RegisterCodec(json.NewCodec(), "application/json;charset=UTF-8")
	if err := server.RegisterService(&Service{vm: vm}, Name); err != nil {
		return nil, err
	}
	return map[string]*common.HTTPHandler{
		"": {
			LockOptions: common.NoLock,
			Handler:     server,
		},
	}, nil
}

// implements "snowmanblock.ChainVM.common.VM"
// for "ext/vm/[vmID]"
func (vm *VM) CreateStaticHandlers() (map[string]*common.HTTPHandler, error) {
	return nil, nil
}

// implements "snowmanblock.ChainVM.commom.VM.AppHandler"
func (vm *VM) AppRequest(nodeID ids.ShortID, requestID uint32, deadline time.Time, request []byte) error {
	// (currently) no app-specific messages
	return nil
}

// implements "snowmanblock.ChainVM.commom.VM.AppHandler"
func (vm *VM) AppRequestFailed(nodeID ids.ShortID, requestID uint32) error {
	// (currently) no app-specific messages
	return nil
}

// implements "snowmanblock.ChainVM.commom.VM.AppHandler"
func (vm *VM) AppResponse(nodeID ids.ShortID, requestID uint32, response []byte) error {
	// (currently) no app-specific messages
	return nil
}

// implements "snowmanblock.ChainVM.commom.VM.AppHandler"
func (vm *VM) AppGossip(nodeID ids.ShortID, msg []byte) error {
	// TODO: gossip txs
	return nil
}

// implements "snowmanblock.ChainVM.commom.VM.health.Checkable"
func (vm *VM) HealthCheck() (interface{}, error) {
	return http.StatusOK, nil
}

// implements "snowmanblock.ChainVM.commom.VM.validators.Connector"
func (vm *VM) Connected(id ids.ShortID) error {
	// no-op
	return nil
}

// implements "snowmanblock.ChainVM.commom.VM.validators.Connector"
func (vm *VM) Disconnected(id ids.ShortID) error {
	// no-op
	return nil
}

// implements "snowmanblock.ChainVM.commom.VM.Getter"
// replaces "core.SnowmanVM.GetBlock"
func (vm *VM) GetBlock(id ids.ID) (snowman.Block, error) {
	b, err := vm.getBlock(id)
	if err != nil {
		log.Warn("failed to get block", "err", err)
	}
	return b, err
}

func (vm *VM) getBlock(blkID ids.ID) (*chain.Block, error) {
	if blk, exists := vm.verifiedBlocks[blkID]; exists {
		return blk, nil
	}

	// Need to initialize for parent lookup to work right
	// TODO: clean this up a ton
	b, err := chain.GetBlock(vm.db, blkID)
	if err != nil {
		return nil, err
	}
	// TODO: set accepted here too instead of in chain
	b.SetVM(vm)
	return b, nil
}

func (vm *VM) readWindow(currTime int64, lastID ids.ID, f func(b *chain.Block) bool) {
	curr, err := vm.getBlock(lastID)
	if err != nil {
		panic(err)
	}
	// Include at least parent block in the window, regardless of how old (TODO:
	// should we change that?)
	for curr != nil && (currTime-curr.Tmstmp <= chain.LookbackWindow || curr.ID() == lastID) {
		if !f(curr) {
			return
		}
		if curr.Prnt == (ids.ID{}) /* genesis */ {
			return
		}
		b, err := vm.getBlock(curr.Prnt)
		if err != nil {
			panic(err)
		}
		curr = b
	}
}

func (vm *VM) ValidBlockID(blockID ids.ID) bool {
	var foundBlockID bool
	vm.readWindow(time.Now().Unix(), vm.preferred, func(b *chain.Block) bool {
		if b.ID() == blockID {
			foundBlockID = true
			return false
		}
		return true
	})
	return foundBlockID
}

func (vm *VM) DifficultyEstimate() uint64 {
	totalDifficulty := uint64(0)
	totalBlocks := uint64(0)
	vm.readWindow(time.Now().Unix(), vm.preferred, func(b *chain.Block) bool {
		totalDifficulty += b.Difficulty
		totalBlocks++
		return true
	})
	return totalDifficulty/totalBlocks + 1
}

func (vm *VM) Recents(currTime int64, lastBlock *chain.Block) (ids.Set, ids.Set, uint64, uint64) {
	recentBlockIDs := ids.Set{}
	recentTxIDs := ids.Set{}
	vm.readWindow(currTime, lastBlock.ID(), func(b *chain.Block) bool {
		recentBlockIDs.Add(b.ID())
		for _, tx := range b.Txs {
			recentTxIDs.Add(tx.ID())
		}
		return true
	})

	// compute new block cost
	secondsSinceLast := currTime - lastBlock.Tmstmp
	newBlockCost := lastBlock.Cost
	if newBlockCost < chain.MinBlockCost {
		newBlockCost = chain.MinBlockCost // needed for genesis (TODO: cleanup)
	}
	if secondsSinceLast < chain.BlockTarget {
		newBlockCost += uint64(chain.BlockTarget - secondsSinceLast)
	} else {
		possibleDiff := uint64(secondsSinceLast - chain.BlockTarget)
		if possibleDiff < newBlockCost-chain.MinBlockCost {
			newBlockCost -= possibleDiff
		} else {
			newBlockCost = chain.MinBlockCost
		}
	}

	// compute new min difficulty
	newMinDifficulty := lastBlock.Difficulty
	if newMinDifficulty < chain.MinDifficulty {
		newMinDifficulty = chain.MinDifficulty // needed for genesis (TODO: cleanup)
	}
	recentTxs := recentTxIDs.Len()
	if recentTxs > chain.TargetTransactions {
		newMinDifficulty++
	} else if recentTxs < chain.TargetTransactions {
		elapsedWindows := uint64(secondsSinceLast/chain.LookbackWindow) + 1 // account for current window being less
		if elapsedWindows < newMinDifficulty-chain.MinDifficulty {
			newMinDifficulty -= elapsedWindows
		} else {
			newMinDifficulty = chain.MinDifficulty
		}
	}

	return recentBlockIDs, recentTxIDs, newBlockCost, newMinDifficulty
}

// implements "snowmanblock.ChainVM.commom.VM.Parser"
// replaces "core.SnowmanVM.ParseBlock"
func (vm *VM) ParseBlock(source []byte) (snowman.Block, error) {
	blk, err := chain.InitializeBlock(
		source,
		choices.Processing,
		vm,
	)
	if blk == nil {
		log.Debug("parsing block", "err", err)
	} else {
		log.Debug("parsing block", "id", blk.ID())
	}
	return blk, err
}

// implements "snowmanblock.ChainVM"
func (vm *VM) BuildBlock() (snowman.Block, error) {
	log.Info("attempting block building")
	vm.l.Lock()
	defer vm.l.Unlock()

	nextTime := time.Now().Unix()
	parent, err := vm.getBlock(vm.preferred)
	if err != nil {
		log.Debug("block building failed: couldn't get parent", "err", err)
		return nil, err
	}
	recentBlockIDs, recentTxIDs, blockCost, minDifficulty := vm.Recents(nextTime, parent)
	b := chain.NewBlock(vm, parent, nextTime, minDifficulty, blockCost)

	// select new transactions
	// TODO: move into chain package
	parentDB, err := parent.OnAccept()
	if err != nil {
		log.Debug("block building failed: couldn't get parent db", "err", err)
		return nil, err
	}
	tdb := versiondb.New(parentDB)
	b.Txs = []*chain.Transaction{}
	vm.mempool.Prune(recentBlockIDs) // clean out invalid txs
	for len(b.Txs) < chain.TargetTransactions && vm.mempool.Len() > 0 {
		next, diff := vm.mempool.PopMax()
		if diff < b.Difficulty {
			vm.mempool.Push(next)
			log.Debug("skipping tx: too low difficulty", "block diff", b.Difficulty, "tx diff", next.Difficulty())
			break
		}
		// Verify that changes pass
		ttdb := versiondb.New(tdb)
		if err := next.Verify(ttdb, b.Tmstmp, recentBlockIDs, recentTxIDs, b.Difficulty); err != nil {
			log.Debug("skipping tx: failed verification", "err", err)
			ttdb.Abort()
			continue
		}
		if err := ttdb.Commit(); err != nil {
			panic(err)
		}
		// Wait to add prefix until after verification
		b.Txs = append(b.Txs, next)
	}
	if err := b.Verify(); err != nil {
		log.Debug("block building failed: failed verification", "err", err)
		return nil, err
	}
	return b, nil
}

func (vm *VM) Submit(tx *chain.Transaction) {
	vm.l.Lock()
	defer vm.l.Unlock()
	// cache difficulty
	_ = tx.Difficulty()
	vm.mempool.Push(tx)

	// TODO: do on a timer
	vm.notifyBlockReady()
}

// "SetPreference" implements "snowmanblock.ChainVM"
// replaces "core.SnowmanVM.SetPreference"
func (vm *VM) SetPreference(id ids.ID) error {
	log.Info("set preference", "id", id)
	vm.preferred = id
	return nil
}

// "LastAccepted" implements "snowmanblock.ChainVM"
// replaces "core.SnowmanVM.LastAccepted"
func (vm *VM) LastAccepted() (ids.ID, error) {
	return vm.lastAccepted, nil
}

func (vm *VM) notifyBlockReady() {
	select {
	case vm.toEngine <- common.PendingTxs:
	default:
		log.Debug("dropping message to consensus engine")
	}
}

// chain.VM
func (vm *VM) State() database.Database {
	return vm.db
}

// TODO: change naming
func (vm *VM) Get(blockID ids.ID) (*chain.Block, error) {
	return vm.getBlock(blockID)
}
func (vm *VM) Verified(b *chain.Block) error {
	if b.Prnt == vm.preferred {
		vm.preferred = b.ID()
	}
	vm.verifiedBlocks[b.ID()] = b
	// TODO: remove txs from mempool (need to be careful not to create a deadlock
	// with BuildBlock)
	log.Info("verified block", "id", b.ID(), "parent", b.Prnt)
	return nil
}
func (vm *VM) Rejected(b *chain.Block) error {
	delete(vm.verifiedBlocks, b.ID())
	// TODO: add txs to mempool
	log.Info("rejected block", "id", b.ID())
	return nil
}
func (vm *VM) Accepted(b *chain.Block) error {
	// TODO: do reorg if preferred not in canonical chain
	vm.lastAccepted = b.ID()
	delete(vm.verifiedBlocks, b.ID())
	log.Info("accepted block", "id", b.ID())
	return nil
}
