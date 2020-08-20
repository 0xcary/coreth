// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package evm

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ava-labs/coreth"
	"github.com/ava-labs/coreth/core"
	"github.com/ava-labs/coreth/core/state"
	"github.com/ava-labs/coreth/core/types"
	"github.com/ava-labs/coreth/eth"
	"github.com/ava-labs/coreth/node"

	"github.com/ava-labs/go-ethereum/common"
	"github.com/ava-labs/go-ethereum/rlp"
	"github.com/ava-labs/go-ethereum/rpc"
	avarpc "github.com/gorilla/rpc/v2"

	"github.com/ava-labs/gecko/api/admin"
	"github.com/ava-labs/gecko/cache"
	"github.com/ava-labs/gecko/database"
	"github.com/ava-labs/gecko/ids"
	"github.com/ava-labs/gecko/snow"
	"github.com/ava-labs/gecko/snow/choices"
	"github.com/ava-labs/gecko/snow/consensus/snowman"
	"github.com/ava-labs/gecko/utils/codec"
	avajson "github.com/ava-labs/gecko/utils/json"
	"github.com/ava-labs/gecko/utils/timer"
	"github.com/ava-labs/gecko/utils/wrappers"
	"github.com/ava-labs/gecko/vms/components/avax"
	"github.com/ava-labs/gecko/vms/secp256k1fx"

	commonEng "github.com/ava-labs/gecko/snow/engine/common"
)

var (
	zeroAddr = common.Address{
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}
)

const (
	lastAcceptedKey = "snowman_lastAccepted"
)

const (
	minBlockTime = 250 * time.Millisecond
	maxBlockTime = 1000 * time.Millisecond
	batchSize    = 250
)

const (
	bdTimerStateMin = iota
	bdTimerStateMax
	bdTimerStateLong
)

const (
	addressSep = "-"
)

var (
	errEmptyBlock      = errors.New("empty block")
	errCreateBlock     = errors.New("couldn't create block")
	errUnknownBlock    = errors.New("unknown block")
	errBlockFrequency  = errors.New("too frequent block issuance")
	errUnsupportedFXs  = errors.New("unsupported feature extensions")
	errInvalidBlock    = errors.New("invalid block")
	errInvalidAddr     = errors.New("invalid hex address")
	errTooManyAtomicTx = errors.New("too many pending atomix txs")
)

func maxDuration(x, y time.Duration) time.Duration {
	if x > y {
		return x
	}
	return y
}

// Codec does serialization and deserialization
var Codec codec.Codec

func init() {
	Codec = codec.NewDefault()

	errs := wrappers.Errs{}
	errs.Add(
		Codec.RegisterType(&UnsignedImportTx{}),
		Codec.RegisterType(&secp256k1fx.TransferInput{}),
		Codec.RegisterType(&secp256k1fx.Input{}),
		Codec.RegisterType(&secp256k1fx.Credential{}),
		Codec.RegisterType(&secp256k1fx.TransferOutput{}),
		Codec.RegisterType(&secp256k1fx.OutputOwners{}),
		Codec.RegisterType(&secp256k1fx.MintOperation{}),
		Codec.RegisterType(&secp256k1fx.MintOutput{}),
	)
	if errs.Errored() {
		panic(errs.Err)
	}
}

// VM implements the snowman.ChainVM interface
type VM struct {
	ctx *snow.Context

	chainID           *big.Int
	networkID         uint64
	genesisHash       common.Hash
	chain             *coreth.ETHChain
	chaindb           Database
	newBlockChan      chan *Block
	networkChan       chan<- commonEng.Message
	newTxPoolHeadChan chan core.NewTxPoolHeadEvent

	txPoolStabilizedHead common.Hash
	txPoolStabilizedOk   chan struct{}
	txPoolStabilizedLock sync.Mutex

	metalock                     sync.Mutex
	blockCache, blockStatusCache cache.LRU
	lastAccepted                 *Block
	writingMetadata              uint32

	bdlock          sync.Mutex
	blockDelayTimer *timer.Timer
	bdTimerState    int8
	bdGenWaitFlag   bool
	bdGenFlag       bool

	genlock               sync.Mutex
	txSubmitChan          <-chan struct{}
	atomicTxSubmitChan    chan struct{}
	codec                 codec.Codec
	clock                 timer.Clock
	avaxAssetID           ids.ID
	avm                   ids.ID
	txFee                 uint64
	pendingAtomicTxs      chan *Tx
	blockAtomicInputCache cache.LRU
}

func (vm *VM) getAtomicTx(block *types.Block) *Tx {
	atx := new(Tx)
	if extdata := block.ExtraData(); extdata != nil {
		if err := vm.codec.Unmarshal(extdata, atx); err != nil {
			panic(err)
		}
	}
	return atx
}

/*
 ******************************************************************************
 ********************************* Snowman API ********************************
 ******************************************************************************
 */

// Initialize implements the snowman.ChainVM interface
func (vm *VM) Initialize(
	ctx *snow.Context,
	db database.Database,
	b []byte,
	toEngine chan<- commonEng.Message,
	fxs []*commonEng.Fx,
) error {
	if len(fxs) > 0 {
		return errUnsupportedFXs
	}

	vm.ctx = ctx
	vm.avaxAssetID = ctx.AVAXAssetID
	vm.avm = ctx.XChainID
	vm.chaindb = Database{db}
	g := new(core.Genesis)
	err := json.Unmarshal(b, g)
	if err != nil {
		return err
	}

	vm.chainID = g.Config.ChainID

	config := eth.DefaultConfig
	config.ManualCanonical = true
	config.Genesis = g
	config.Miner.ManualMining = true
	config.Miner.DisableUncle = true
	if err := config.SetGCMode("archive"); err != nil {
		panic(err)
	}
	nodecfg := node.Config{NoUSB: true}
	chain := coreth.NewETHChain(&config, &nodecfg, nil, vm.chaindb)
	vm.chain = chain
	vm.networkID = config.NetworkId
	chain.SetOnHeaderNew(func(header *types.Header) {
		hid := make([]byte, 32)
		_, err := rand.Read(hid)
		if err != nil {
			panic("cannot generate hid")
		}
		header.Extra = append(header.Extra, hid...)
	})
	chain.SetOnFinalizeAndAssemble(func(state *state.StateDB, txs []*types.Transaction) ([]byte, error) {
		select {
		case atx := <-vm.pendingAtomicTxs:
			for _, to := range atx.UnsignedTx.(*UnsignedImportTx).Outs {
				amount := new(big.Int)
				amount.SetUint64(to.Amount)
				state.AddBalance(to.Address, amount)
			}
			raw, _ := vm.codec.Marshal(atx)
			return raw, nil
		default:
			if len(txs) == 0 {
				// this could happen due to the async logic of geth tx pool
				vm.newBlockChan <- nil
				return nil, errEmptyBlock
			}
		}
		return nil, nil
	})
	chain.SetOnSealFinish(func(block *types.Block) error {
		vm.ctx.Log.Verbo("EVM sealed a block")

		blk := &Block{
			id:       ids.NewID(block.Hash()),
			ethBlock: block,
			vm:       vm,
		}
		if blk.Verify() != nil {
			return errInvalidBlock
		}
		vm.newBlockChan <- blk
		vm.updateStatus(ids.NewID(block.Hash()), choices.Processing)
		vm.txPoolStabilizedLock.Lock()
		vm.txPoolStabilizedHead = block.Hash()
		vm.txPoolStabilizedLock.Unlock()
		return nil
	})
	chain.SetOnQueryAcceptedBlock(func() *types.Block {
		return vm.getLastAccepted().ethBlock
	})
	chain.SetOnExtraStateChange(func(block *types.Block, statedb *state.StateDB) error {
		atx := vm.getAtomicTx(block).UnsignedTx.(*UnsignedImportTx)
		for _, to := range atx.Outs {
			amount := new(big.Int)
			amount.SetUint64(to.Amount)
			statedb.AddBalance(to.Address, amount)
		}
		return nil
	})
	vm.blockCache = cache.LRU{Size: 2048}
	vm.blockStatusCache = cache.LRU{Size: 1024}
	vm.blockAtomicInputCache = cache.LRU{Size: 4096}
	vm.newBlockChan = make(chan *Block)
	vm.networkChan = toEngine
	vm.blockDelayTimer = timer.NewTimer(func() {
		vm.bdlock.Lock()
		switch vm.bdTimerState {
		case bdTimerStateMin:
			vm.bdTimerState = bdTimerStateMax
			vm.blockDelayTimer.SetTimeoutIn(maxDuration(maxBlockTime-minBlockTime, 0))
		case bdTimerStateMax:
			vm.bdTimerState = bdTimerStateLong
		}
		tryAgain := vm.bdGenWaitFlag
		vm.bdlock.Unlock()
		if tryAgain {
			vm.tryBlockGen()
		}
	})
	go ctx.Log.RecoverAndPanic(vm.blockDelayTimer.Dispatch)

	vm.bdTimerState = bdTimerStateLong
	vm.bdGenWaitFlag = true
	vm.newTxPoolHeadChan = make(chan core.NewTxPoolHeadEvent, 1)
	vm.txPoolStabilizedOk = make(chan struct{}, 1)
	// TODO: read size from options
	vm.pendingAtomicTxs = make(chan *Tx, 1024)
	vm.atomicTxSubmitChan = make(chan struct{}, 1)
	chain.GetTxPool().SubscribeNewHeadEvent(vm.newTxPoolHeadChan)
	// TODO: shutdown this go routine
	go ctx.Log.RecoverAndPanic(func() {
		for {
			select {
			case h := <-vm.newTxPoolHeadChan:
				vm.txPoolStabilizedLock.Lock()
				if vm.txPoolStabilizedHead == h.Block.Hash() {
					vm.txPoolStabilizedOk <- struct{}{}
					vm.txPoolStabilizedHead = common.Hash{}
				}
				vm.txPoolStabilizedLock.Unlock()
			}
		}
	})
	chain.Start()

	var lastAccepted *types.Block
	if b, err := vm.chaindb.Get([]byte(lastAcceptedKey)); err == nil {
		var hash common.Hash
		if err = rlp.DecodeBytes(b, &hash); err == nil {
			if block := chain.GetBlockByHash(hash); block == nil {
				vm.ctx.Log.Debug("lastAccepted block not found in chaindb")
			} else {
				lastAccepted = block
			}
		}
	}
	if lastAccepted == nil {
		vm.ctx.Log.Debug("lastAccepted is unavailable, setting to the genesis block")
		lastAccepted = chain.GetGenesisBlock()
	}
	vm.lastAccepted = &Block{
		id:       ids.NewID(lastAccepted.Hash()),
		ethBlock: lastAccepted,
		vm:       vm,
	}
	vm.genesisHash = chain.GetGenesisBlock().Hash()
	vm.ctx.Log.Info(fmt.Sprintf("lastAccepted = %s", vm.lastAccepted.ethBlock.Hash().Hex()))

	// TODO: shutdown this go routine
	go vm.ctx.Log.RecoverAndPanic(func() {
		vm.txSubmitChan = vm.chain.GetTxSubmitCh()
		for {
			select {
			case <-vm.txSubmitChan:
				vm.ctx.Log.Verbo("New tx detected, trying to generate a block")
				vm.tryBlockGen()
			case <-vm.atomicTxSubmitChan:
				vm.ctx.Log.Verbo("New atomic Tx detected, trying to generate a block")
				vm.tryBlockGen()
			case <-time.After(5 * time.Second):
				vm.tryBlockGen()
			}
		}
	})
	vm.codec = Codec

	return nil
}

// Bootstrapping notifies this VM that the consensus engine is performing
// bootstrapping
func (vm *VM) Bootstrapping() error { return nil }

// Bootstrapped notifies this VM that the consensus engine has finished
// bootstrapping
func (vm *VM) Bootstrapped() error { return nil }

// Shutdown implements the snowman.ChainVM interface
func (vm *VM) Shutdown() error {
	if vm.ctx == nil {
		return nil
	}

	vm.writeBackMetadata()
	vm.chain.Stop()
	return nil
}

// BuildBlock implements the snowman.ChainVM interface
func (vm *VM) BuildBlock() (snowman.Block, error) {
	vm.chain.GenBlock()
	block := <-vm.newBlockChan
	if block == nil {
		return nil, errCreateBlock
	}
	// reset the min block time timer
	vm.bdlock.Lock()
	vm.bdTimerState = bdTimerStateMin
	vm.bdGenWaitFlag = false
	vm.bdGenFlag = false
	vm.blockDelayTimer.SetTimeoutIn(minBlockTime)
	vm.bdlock.Unlock()

	vm.ctx.Log.Debug("built block 0x%x", block.ID().Bytes())
	// make sure Tx Pool is updated
	<-vm.txPoolStabilizedOk
	return block, nil
}

// ParseBlock implements the snowman.ChainVM interface
func (vm *VM) ParseBlock(b []byte) (snowman.Block, error) {
	vm.metalock.Lock()
	defer vm.metalock.Unlock()

	ethBlock := new(types.Block)
	if err := rlp.DecodeBytes(b, ethBlock); err != nil {
		return nil, err
	}
	if !vm.chain.VerifyBlock(ethBlock) {
		return nil, errInvalidBlock
	}
	blockHash := ethBlock.Hash()
	// Coinbase must be zero on C-Chain
	if bytes.Compare(blockHash.Bytes(), vm.genesisHash.Bytes()) != 0 &&
		bytes.Compare(ethBlock.Coinbase().Bytes(), coreth.BlackholeAddr.Bytes()) != 0 {
		return nil, errInvalidBlock
	}
	block := &Block{
		id:       ids.NewID(blockHash),
		ethBlock: ethBlock,
		vm:       vm,
	}
	vm.blockCache.Put(block.ID(), block)
	return block, nil
}

// GetBlock implements the snowman.ChainVM interface
func (vm *VM) GetBlock(id ids.ID) (snowman.Block, error) {
	vm.metalock.Lock()
	defer vm.metalock.Unlock()

	block := vm.getBlock(id)
	if block == nil {
		return nil, errUnknownBlock
	}
	return block, nil
}

// SetPreference sets what the current tail of the chain is
func (vm *VM) SetPreference(blkID ids.ID) {
	err := vm.chain.SetTail(blkID.Key())
	vm.ctx.Log.AssertNoError(err)
}

// LastAccepted returns the ID of the block that was last accepted
func (vm *VM) LastAccepted() ids.ID {
	vm.metalock.Lock()
	defer vm.metalock.Unlock()

	return vm.lastAccepted.ID()
}

// NewHandler returns a new Handler for a service where:
//   * The handler's functionality is defined by [service]
//     [service] should be a gorilla RPC service (see https://www.gorillatoolkit.org/pkg/rpc/v2)
//   * The name of the service is [name]
//   * The LockOption is the first element of [lockOption]
//     By default the LockOption is WriteLock
//     [lockOption] should have either 0 or 1 elements. Elements beside the first are ignored.
func newHandler(name string, service interface{}, lockOption ...commonEng.LockOption) *commonEng.HTTPHandler {
	server := avarpc.NewServer()
	server.RegisterCodec(avajson.NewCodec(), "application/json")
	server.RegisterCodec(avajson.NewCodec(), "application/json;charset=UTF-8")
	server.RegisterService(service, name)

	var lock commonEng.LockOption = commonEng.WriteLock
	if len(lockOption) != 0 {
		lock = lockOption[0]
	}
	return &commonEng.HTTPHandler{LockOptions: lock, Handler: server}
}

// CreateHandlers makes new http handlers that can handle API calls
func (vm *VM) CreateHandlers() map[string]*commonEng.HTTPHandler {
	handler := vm.chain.NewRPCHandler()
	vm.chain.AttachEthService(handler, []string{"eth", "personal", "txpool"})
	handler.RegisterName("net", &NetAPI{vm})
	handler.RegisterName("snowman", &SnowmanAPI{vm})
	handler.RegisterName("web3", &Web3API{})
	handler.RegisterName("debug", &DebugAPI{vm})
	handler.RegisterName("admin", &admin.Performance{})

	return map[string]*commonEng.HTTPHandler{
		"/rpc": &commonEng.HTTPHandler{LockOptions: commonEng.NoLock, Handler: handler},
		"/ava": newHandler("ava", &AvaAPI{vm}),
		"/ws":  &commonEng.HTTPHandler{LockOptions: commonEng.NoLock, Handler: handler.WebsocketHandler([]string{"*"})},
	}
}

// CreateStaticHandlers makes new http handlers that can handle API calls
func (vm *VM) CreateStaticHandlers() map[string]*commonEng.HTTPHandler {
	handler := rpc.NewServer()
	handler.RegisterName("static", &StaticService{})
	return map[string]*commonEng.HTTPHandler{
		"/rpc": &commonEng.HTTPHandler{LockOptions: commonEng.NoLock, Handler: handler},
		"/ws":  &commonEng.HTTPHandler{LockOptions: commonEng.NoLock, Handler: handler.WebsocketHandler([]string{"*"})},
	}
}

/*
 ******************************************************************************
 *********************************** Helpers **********************************
 ******************************************************************************
 */

func (vm *VM) updateStatus(blockID ids.ID, status choices.Status) {
	vm.metalock.Lock()
	defer vm.metalock.Unlock()

	if status == choices.Accepted {
		vm.lastAccepted = vm.getBlock(blockID)
		// TODO: improve this naive implementation
		if atomic.SwapUint32(&vm.writingMetadata, 1) == 0 {
			go vm.ctx.Log.RecoverAndPanic(vm.writeBackMetadata)
		}
	}
	vm.blockStatusCache.Put(blockID, status)
}

func (vm *VM) getCachedBlock(blockID ids.ID) *types.Block {
	return vm.chain.GetBlockByHash(blockID.Key())
}

func (vm *VM) tryBlockGen() error {
	vm.bdlock.Lock()
	defer vm.bdlock.Unlock()
	if vm.bdGenFlag {
		// skip if one call already generates a block in this round
		return nil
	}
	vm.bdGenWaitFlag = true

	vm.genlock.Lock()
	defer vm.genlock.Unlock()
	// get pending size
	size, err := vm.chain.PendingSize()
	if err != nil {
		return err
	}
	if size == 0 && len(vm.pendingAtomicTxs) == 0 {
		return nil
	}

	switch vm.bdTimerState {
	case bdTimerStateMin:
		return nil
	case bdTimerStateMax:
		if size < batchSize {
			return nil
		}
	case bdTimerStateLong:
		// timeout; go ahead and generate a new block anyway
	}
	select {
	case vm.networkChan <- commonEng.PendingTxs:
		// successfully push out the notification; this round ends
		vm.bdGenFlag = true
	default:
		return errBlockFrequency
	}
	return nil
}

func (vm *VM) getCachedStatus(blockID ids.ID) choices.Status {
	vm.metalock.Lock()
	defer vm.metalock.Unlock()
	status := choices.Processing

	if statusIntf, ok := vm.blockStatusCache.Get(blockID); ok {
		status = statusIntf.(choices.Status)
	} else {
		blk := vm.chain.GetBlockByHash(blockID.Key())
		if blk == nil {
			return choices.Unknown
		}
		acceptedBlk := vm.lastAccepted.ethBlock

		// TODO: There must be a better way of doing this.
		// Traverse up the chain from the lower block until the indices match
		highBlock := blk
		lowBlock := acceptedBlk
		if highBlock.Number().Cmp(lowBlock.Number()) < 0 {
			highBlock, lowBlock = lowBlock, highBlock
		}
		for highBlock.Number().Cmp(lowBlock.Number()) > 0 {
			highBlock = vm.chain.GetBlockByHash(highBlock.ParentHash())
		}

		if highBlock.Hash() == lowBlock.Hash() { // on the same branch
			if blk.Number().Cmp(acceptedBlk.Number()) <= 0 {
				status = choices.Accepted
			}
		} else { // on different branches
			status = choices.Rejected
		}
	}

	vm.blockStatusCache.Put(blockID, status)
	return status
}

func (vm *VM) getBlock(id ids.ID) *Block {
	if blockIntf, ok := vm.blockCache.Get(id); ok {
		return blockIntf.(*Block)
	}
	ethBlock := vm.getCachedBlock(id)
	if ethBlock == nil {
		return nil
	}
	block := &Block{
		id:       ids.NewID(ethBlock.Hash()),
		ethBlock: ethBlock,
		vm:       vm,
	}
	vm.blockCache.Put(id, block)
	return block
}

func (vm *VM) issueRemoteTxs(txs []*types.Transaction) error {
	errs := vm.chain.AddRemoteTxs(txs)
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return vm.tryBlockGen()
}

func (vm *VM) writeBackMetadata() {
	vm.metalock.Lock()
	defer vm.metalock.Unlock()

	b, err := rlp.EncodeToBytes(vm.lastAccepted.ethBlock.Hash())
	if err != nil {
		vm.ctx.Log.Error("snowman-eth: error while writing back metadata")
		return
	}
	vm.ctx.Log.Debug("writing back metadata")
	vm.chaindb.Put([]byte(lastAcceptedKey), b)
	atomic.StoreUint32(&vm.writingMetadata, 0)
}

func (vm *VM) getLastAccepted() *Block {
	vm.metalock.Lock()
	defer vm.metalock.Unlock()

	return vm.lastAccepted
}

// ParseLocalAddress takes in an address for this chain and produces the ID
func (vm *VM) ParseLocalAddress(addrStr string) (common.Address, error) {
	if !common.IsHexAddress(addrStr) {
		return common.Address{}, errInvalidAddr
	}
	return common.HexToAddress(addrStr), nil
}

func (vm *VM) FormatAddress(addr common.Address) (string, error) {
	return addr.Hex(), nil
}

func (vm *VM) issueTx(tx *Tx) error {
	select {
	case vm.pendingAtomicTxs <- tx:
		select {
		case vm.atomicTxSubmitChan <- struct{}{}:
		default:
		}
	default:
		return errTooManyAtomicTx
	}
	return nil
}

// GetAtomicUTXOs returns the utxos that at least one of the provided addresses is
// referenced in.
func (vm *VM) GetAtomicUTXOs(
	chainID ids.ID,
	addrs ids.ShortSet,
	startAddr ids.ShortID,
	startUTXOID ids.ID,
	limit int,
) ([]*avax.UTXO, ids.ShortID, ids.ID, error) {
	// TODO: finish this function via gRPC
	utxos := []*avax.UTXO{{
		UTXOID: avax.UTXOID{TxID: ids.Empty},
		Asset:  avax.Asset{ID: vm.ctx.AVAXAssetID},
		Out: &secp256k1fx.TransferOutput{
			Amt: 100,
		},
	}}
	return utxos, ids.ShortEmpty, ids.Empty, nil
}
