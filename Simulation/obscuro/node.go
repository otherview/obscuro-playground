package obscuro

import (
	"fmt"
	"github.com/google/uuid"
	"simulation/common"
	"simulation/ethereum-mock"
	common2 "simulation/obscuro/common"
	"simulation/obscuro/enclave"
	"sync/atomic"
	"time"
)

type AggregatorCfg struct {
	// duration of the gossip round
	GossipRoundDuration uint64
}

type L2Network interface {
	BroadcastRollup(r common.EncodedRollup)
	BroadcastTx(tx common2.EncryptedTx)
}

type StatsCollector interface {
	// Register when a node has to discard the speculative work built on top of the winner of the gossip round.
	L2Recalc(id common.NodeId)
	NewBlock(block *common.Block)
	NewRollup(rollup *common2.Rollup)
	RollupWithMoreRecentProof()
}

// Node this will become the Obscuro "Node" type
type Node struct {
	Id common.NodeId

	l2Network L2Network
	L1Node    *ethereum_mock.Node

	mining  bool // true -if this is an aggregator, false if it is a validator
	genesis bool // true - if this is the first Obscuro node which has to initialize the network
	cfg     AggregatorCfg

	stats StatsCollector

	// control the lifecycle
	exitNodeCh chan bool
	interrupt  *int32

	// where the connected L1Node node drops new blocks
	blockRpcCh chan common.EncodedBlock
	forkRpcCh  chan []common.EncodedBlock

	rollupsP2pCh chan common.EncodedRollup

	// Interface to the logic running inside the TEE
	Enclave enclave.Enclave
}

func (a *Node) Start() {
	// Todo: This is a naive implementation.
	// It feeds the entire L1 blockchain into the enclave when it starts
	allblocks := a.L1Node.RPCBlockchainFeed()
	extblocks := make([]common.ExtBlock, len(allblocks))
	for i, b := range allblocks {
		extblocks[i] = b.ToExtBlock()
	}
	a.Enclave.IngestBlocks(extblocks)
	//todo - what happens with the blocks received while processing ?
	go a.Enclave.Start(extblocks[len(extblocks)-1])

	if a.genesis {
		a.initialiseProtocol()
	}

	// used as a signaling mechanism to stop processing the old block if a new L1 block arrives earlier
	i := int32(0)
	var interrupt = &i

	// Main loop - Listen for notifications From the L1 node and process them
	// Note that during processing, more recent notifications can be received.
	for {
		select {
		case b := <-a.blockRpcCh:
			interrupt = sendInterrupt(interrupt)
			a.processBlocks([]common.EncodedBlock{b}, interrupt)

		case f := <-a.forkRpcCh:
			interrupt = sendInterrupt(interrupt)
			a.processBlocks(f, interrupt)

		case r := <-a.rollupsP2pCh:
			rol, _ := common2.Decode(r)
			go a.Enclave.SubmitRollup(common2.ExtRollup{
				Header: rol.Header,
				Txs:    rol.Transactions,
			})

		case <-a.exitNodeCh:
			a.Enclave.Stop()
			return
		}
	}
}

func sendInterrupt(interrupt *int32) *int32 {
	// Notify the previous round to stop work
	atomic.StoreInt32(interrupt, 1)
	i := int32(0)
	return &i
}

func (a *Node) processBlocks(blocks []common.EncodedBlock, interrupt *int32) {
	var result enclave.SubmitBlockResponse
	for _, block := range blocks {
		result = a.Enclave.SubmitBlock(block.DecodeBlock().ToExtBlock())
	}

	if !result.Processed {
		b := blocks[len(blocks)-1].DecodeBlock()
		common.Log(fmt.Sprintf(">   Agg%d: Could not process block b_%s", a.Id, common.Str(b.Hash())))
		return
	}
	a.l2Network.BroadcastRollup(common2.EncodeRollup(result.Rollup.ToRollup()))

	common.ScheduleInterrupt(a.cfg.GossipRoundDuration, interrupt, func() {
		if atomic.LoadInt32(a.interrupt) == 1 {
			return
		}
		// Request the round winner for the current head
		winnerRollup, submit := a.Enclave.RoundWinner(result.Hash)
		if submit {
			tx := common.L1Tx{Id: uuid.New(), TxType: common.RollupTx, Rollup: common2.EncodeRollup(winnerRollup.ToRollup())}
			t, err := tx.Encode()
			if err != nil {
				panic(err)
			}
			a.L1Node.BroadcastTx(t)
			// collect Stats
			//a.stats.NewRollup(DecodeRollup(winnerRollup))
		}
	})
}

// RPCNewHead Receive notifications From the L1Node Node when there's a new block
func (a *Node) RPCNewHead(b common.EncodedBlock) {
	if atomic.LoadInt32(a.interrupt) == 1 {
		return
	}
	a.blockRpcCh <- b
}

func (a *Node) RPCNewFork(b []common.EncodedBlock) {
	if atomic.LoadInt32(a.interrupt) == 1 {
		return
	}
	a.forkRpcCh <- b
}

// P2PGossipRollup is called by counterparties when there is a Rollup to broadcast
// All it does is forward the rollup for processing to the enclave
func (a *Node) P2PGossipRollup(r common.EncodedRollup) {
	if atomic.LoadInt32(a.interrupt) == 1 {
		return
	}
	a.rollupsP2pCh <- r
}

func (a *Node) P2PReceiveTx(tx common2.EncryptedTx) {
	if atomic.LoadInt32(a.interrupt) == 1 {
		return
	}
	go a.Enclave.SubmitTx(tx)
}

func (a *Node) RPCBalance(address common.Address) uint64 {
	return a.Enclave.Balance(address)
}

func (a *Node) Stop() {
	// block all requests
	atomic.StoreInt32(a.interrupt, 1)
	a.Enclave.Stop()
	time.Sleep(time.Millisecond * 10)
	a.exitNodeCh <- true
}

// Called only by the first enclave to bootstrap the network
func (a *Node) initialiseProtocol() common.L2RootHash {
	//todo shared secret
	genesis := a.Enclave.ProduceGenesis()
	tx := common.L1Tx{Id: uuid.New(), TxType: common.RollupTx, Rollup: common2.EncodeRollup(genesis.Rollup.ToRollup())}
	t, err := tx.Encode()
	if err != nil {
		panic(err)
	}
	a.L1Node.BroadcastTx(t)
	return genesis.Hash
}

func NewAgg(id common.NodeId, cfg AggregatorCfg, l1 *ethereum_mock.Node, l2Network L2Network, collector StatsCollector, genesis bool) Node {
	return Node{
		// config
		Id:        id,
		cfg:       cfg,
		mining:    true,
		genesis:   genesis,
		L1Node:    l1,
		l2Network: l2Network,

		stats: collector,

		// lifecycle channels
		exitNodeCh: make(chan bool),
		interrupt:  new(int32),

		// incoming data
		blockRpcCh:   make(chan common.EncodedBlock),
		forkRpcCh:    make(chan []common.EncodedBlock),
		rollupsP2pCh: make(chan common.EncodedRollup),

		// State processing
		Enclave: enclave.NewEnclave(id, true, collector),
	}
}
