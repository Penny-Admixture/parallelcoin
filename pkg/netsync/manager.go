package netsync

import (
	"container/list"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	block2 "github.com/p9c/matrjoska/pkg/block"

	"github.com/p9c/matrjoska/pkg/qu"

	"github.com/p9c/matrjoska/pkg/blockchain"
	"github.com/p9c/matrjoska/pkg/chaincfg"
	"github.com/p9c/matrjoska/pkg/chainhash"
	"github.com/p9c/matrjoska/pkg/database"
	"github.com/p9c/matrjoska/pkg/mempool"
	peerpkg "github.com/p9c/matrjoska/pkg/peer"
	"github.com/p9c/matrjoska/pkg/util"
	"github.com/p9c/matrjoska/pkg/wire"
)

type (
	// SyncManager is used to communicate block related messages with peers. The
	// SyncManager is started as by executing Start() in a goroutine. Once started,
	// it selects peers to sync from and starts the initial block download. Once the
	// chain is in sync, the SyncManager handles incoming block and header
	// notifications and relays announcements of new blocks to peers.
	SyncManager struct {
		peerNotifier   PeerNotifier
		started        int32
		shutdown       int32
		chain          *blockchain.BlockChain
		txMemPool      *mempool.TxPool
		chainParams    *chaincfg.Params
		progressLogger *blockProgressLogger
		msgChan        chan interface{}
		wg             sync.WaitGroup
		quit           qu.C
		// These fields should only be accessed from the blockHandler thread
		rejectedTxns    map[chainhash.Hash]struct{}
		requestedTxns   map[chainhash.Hash]struct{}
		requestedBlocks map[chainhash.Hash]struct{}
		syncPeer        *peerpkg.Peer
		peerStates      map[*peerpkg.Peer]*peerSyncState
		// The following fields are used for headers-first mode.
		headersFirstMode bool
		headerList       *list.List
		startHeader      *list.Element
		nextCheckpoint   *chaincfg.Checkpoint
		// An optional fee estimator.
		feeEstimator *mempool.FeeEstimator
	}
	// blockMsg packages a bitcoin block message and the peer it came from together
	// so the block handler has access to that information.
	blockMsg struct {
		block *block2.Block
		peer  *peerpkg.Peer
		reply qu.C
	}
	// donePeerMsg signifies a newly disconnected peer to the block handler.
	donePeerMsg struct {
		peer *peerpkg.Peer
	}
	// getSyncPeerMsg is a message type to be sent across the message channel for
	// retrieving the current sync peer.
	getSyncPeerMsg struct {
		reply chan int32
	}
	// headerNode is used as a node in a list of headers that are linked together
	// between checkpoints.
	headerNode struct {
		height int32
		hash   *chainhash.Hash
	}
	// headersMsg packages a bitcoin headers message and the peer it came from
	// together so the block handler has access to that information.
	headersMsg struct {
		headers *wire.MsgHeaders
		peer    *peerpkg.Peer
	}
	// invMsg packages a bitcoin inv message and the peer it came from together so
	// the block handler has access to that information.
	invMsg struct {
		inv  *wire.MsgInv
		peer *peerpkg.Peer
	}
	// isCurrentMsg is a message type to be sent across the message channel for
	// requesting whether or not the sync manager believes it is synced with the
	// currently connected peers.
	isCurrentMsg struct {
		reply chan bool
	}
	// newPeerMsg signifies a newly connected peer to the block handler.
	newPeerMsg struct {
		peer *peerpkg.Peer
	}
	// pauseMsg is a message type to be sent across the message channel for pausing
	// the sync manager. This effectively provides the caller with exclusive access
	// over the manager until a receive is performed on the unpause channel.
	pauseMsg struct {
		unpause qu.C
	}
	// peerSyncState stores additional information that the SyncManager tracks about
	// a peer.
	peerSyncState struct {
		syncCandidate   bool
		requestQueue    []*wire.InvVect
		requestedTxns   map[chainhash.Hash]struct{}
		requestedBlocks map[chainhash.Hash]struct{}
	}
	// processBlockMsg is a message type to be sent across the message channel for
	// requested a block is processed. Note this call differs from blockMsg above in
	// that blockMsg is intended for blocks that came from peers and have extra
	// handling whereas this message essentially is just a concurrent safe way to
	// call ProcessBlock on the internal block chain instance.
	processBlockMsg struct {
		block *block2.Block
		flags blockchain.BehaviorFlags
		reply chan processBlockResponse
	}
	// processBlockResponse is a response sent to the reply channel of a processBlockMsg.
	processBlockResponse struct {
		isOrphan bool
		err      error
	}
	// txMsg packages a bitcoin tx message and the peer it came from together so the
	// block handler has access to that information.
	txMsg struct {
		tx    *util.Tx
		peer  *peerpkg.Peer
		reply qu.C
	}
)

const (
	// minInFlightBlocks is the minimum number of blocks that should be in the
	// request queue for headers-first mode before requesting more.
	minInFlightBlocks = 10
	// maxRejectedTxns is the maximum number of rejected transactions hashes to
	// store in memory.
	maxRejectedTxns = 1000
	// maxRequestedBlocks is the maximum number of requested block hashes to store
	// in memory.
	maxRequestedBlocks = wire.MaxInvPerMsg
	// maxRequestedTxns is the maximum number of requested transactions hashes to
	// store in memory.
	maxRequestedTxns = wire.MaxInvPerMsg
)

// zeroHash is the zero value hash (all zeros)
var zeroHash chainhash.Hash

// DonePeer informs the blockmanager that a peer has disconnected.
func (sm *SyncManager) DonePeer(peer *peerpkg.Peer) {
	// Ignore if we are shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}
	sm.msgChan <- &donePeerMsg{peer: peer}
}

// IsCurrent returns whether or not the sync manager believes it is synced with
// the connected peers.
func (sm *SyncManager) IsCurrent() bool {
	reply := make(chan bool)
	sm.msgChan <- isCurrentMsg{reply: reply}
	return <-reply
}

// NewPeer informs the sync manager of a newly active peer.
func (sm *SyncManager) NewPeer(peer *peerpkg.Peer) {
	// Ignore if we are shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}
	sm.msgChan <- &newPeerMsg{peer: peer}
}

// Pause pauses the sync manager until the returned channel is closed.
//
// Note that while paused, all peer and block processing is halted. The message
// sender should avoid pausing the sync manager for long durations.
func (sm *SyncManager) Pause() chan<- struct{} {
	c := qu.T()
	sm.msgChan <- pauseMsg{c}
	return c
}

// ProcessBlock makes use of ProcessBlock on an internal instance of a block
// chain.
func (sm *SyncManager) ProcessBlock(block *block2.Block, flags blockchain.BehaviorFlags) (bool, error) {
	T.Ln("processing block")
	// Traces(block)
	reply := make(chan processBlockResponse, 1)
	T.Ln("sending to msgChan")
	sm.msgChan <- processBlockMsg{block: block, flags: flags, reply: reply}
	T.Ln("waiting on reply")
	response := <-reply
	return response.isOrphan, response.err
}

// QueueBlock adds the passed block message and peer to the block handling
// queue. Responds to the done channel argument after the block message is
// processed.
func (sm *SyncManager) QueueBlock(block *block2.Block, peer *peerpkg.Peer, done qu.C) {
	// Don't accept more blocks if we're shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		done <- struct{}{}
		return
	}
	sm.msgChan <- &blockMsg{block: block, peer: peer, reply: done}
}

// QueueHeaders adds the passed headers message and peer to the block handling
// queue.
func (sm *SyncManager) QueueHeaders(headers *wire.MsgHeaders, peer *peerpkg.Peer) {
	// No channel handling here because peers do not need to block on headers
	// messages.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}
	sm.msgChan <- &headersMsg{headers: headers, peer: peer}
}

// QueueInv adds the passed inv message and peer to the block handling queue.
func (sm *SyncManager) QueueInv(inv *wire.MsgInv, peer *peerpkg.Peer) {
	// No channel handling here because peers do not need to block on inv messages.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}
	sm.msgChan <- &invMsg{inv: inv, peer: peer}
}

// QueueTx adds the passed transaction message and peer to the block handling
// queue. Responds to the done channel argument after the tx message is
// processed.
func (sm *SyncManager) QueueTx(tx *util.Tx, peer *peerpkg.Peer, done qu.C) {
	// Don't accept more transactions if we're shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		done <- struct{}{}
		return
	}
	sm.msgChan <- &txMsg{tx: tx, peer: peer, reply: done}
}

// Start begins the core block handler which processes block and inv messages.
func (sm *SyncManager) Start() {
	// Already started?
	if atomic.AddInt32(&sm.started, 1) != 1 {
		return
	}
	T.Ln("starting sync manager")
	sm.wg.Add(1)
	go sm.blockHandler(0)
}

// Stop gracefully shuts down the sync manager by stopping all asynchronous
// handlers and waiting for them to finish.
func (sm *SyncManager) Stop() (e error) {
	if atomic.AddInt32(&sm.shutdown, 1) != 1 {
		D.Ln("sync manager is already in the process of shutting down")
		return nil
	}
	// DEBUG{"sync manager shutting down"}
	sm.quit.Q()
	sm.wg.Wait()
	return nil
}

// SyncPeerID returns the ID of the current sync peer, or 0 if there is none.
func (sm *SyncManager) SyncPeerID() int32 {
	reply := make(chan int32)
	sm.msgChan <- getSyncPeerMsg{reply: reply}
	return <-reply
}

// blockHandler is the main handler for the sync manager. It must be run as a
// goroutine. It processes block and inv messages in a separate goroutine from
// the peer handlers so the block (Block) messages are handled by a single
// thread without needing to lock memory data structures. This is important
// because the sync manager controls which blocks are needed and how the
// fetching should proceed.
func (sm *SyncManager) blockHandler(workerNumber uint32) {
out:
	for {
		select {
		case m := <-sm.msgChan:
			switch msg := m.(type) {
			case *newPeerMsg:
				sm.handleNewPeerMsg(msg.peer)
			case *txMsg:
				sm.handleTxMsg(msg)
				msg.reply <- struct{}{}
			case *blockMsg:
				sm.handleBlockMsg(0, msg)
				msg.reply <- struct{}{}
			case *invMsg:
				sm.handleInvMsg(msg)
			case *headersMsg:
				sm.handleHeadersMsg(msg)
			case *donePeerMsg:
				sm.handleDonePeerMsg(msg.peer)
			case getSyncPeerMsg:
				var peerID int32
				if sm.syncPeer != nil {
					peerID = sm.syncPeer.ID()
				}
				msg.reply <- peerID
			case processBlockMsg:
				T.Ln("received processBlockMsg")
				var heightUpdate int32
				header := &msg.block.WireBlock().Header
				T.Ln("checking if have should have serialized block height")
				if blockchain.ShouldHaveSerializedBlockHeight(header) {
					T.Ln("reading coinbase transaction")
					mbt := msg.block.Transactions()
					if len(mbt) > 0 {
						coinbaseTx := mbt[len(mbt)-1]
						T.Ln("extracting coinbase height")
						var e error
						var cbHeight int32
						if cbHeight, e = blockchain.ExtractCoinbaseHeight(coinbaseTx); E.Chk(e) {
							W.Ln("unable to extract height from coinbase tx:", e)
						} else {
							heightUpdate = cbHeight
						}
					} else {
						D.Ln("no transactions in block??")
					}
				}
				T.Ln("passing to chain.ProcessBlock")
				var isOrphan bool
				var e error
				if _, isOrphan, e = sm.chain.ProcessBlock(
					workerNumber,
					msg.block,
					msg.flags,
					heightUpdate,
				); D.Chk(e) {
					D.Ln("error processing new block ", e)
					msg.reply <- processBlockResponse{
						isOrphan: false,
						err:      e,
					}
				}
				T.Ln("sending back message on reply channel")
				msg.reply <- processBlockResponse{
					isOrphan: isOrphan,
					err:      nil,
				}
				T.Ln("sent reply")
			case isCurrentMsg:
				msg.reply <- sm.current()
			case pauseMsg:
				// Wait until the sender unpauses the manager.
				<-msg.unpause
			default:
				T.F("invalid message type in block handler: %Ter", msg)
			}
		case <-sm.quit.Wait():
			break out
		}
	}
	sm.wg.Done()
}

// current returns true if we believe we are synced with our peers, false if we
// still have blocks to check
func (sm *SyncManager) current() bool {
	if !sm.chain.IsCurrent() {
		return false
	}
	// if blockChain thinks we are current and we have no syncPeer it is probably
	// right.
	if sm.syncPeer == nil {
		return true
	}
	// No matter what chain thinks, if we are below the block we are syncing to we
	// are not current.
	if sm.chain.BestSnapshot().Height < sm.syncPeer.LastBlock() {
		return false
	}
	return true
}

// fetchHeaderBlocks creates and sends a request to the syncPeer for the next
// list of blocks to be downloaded based on the current list of headers.
func (sm *SyncManager) fetchHeaderBlocks() {
	// Nothing to do if there is no start header.
	if sm.startHeader == nil {
		D.Ln("fetchHeaderBlocks called with no start header")
		return
	}
	// Build up a getdata request for the list of blocks the headers describe. The
	// size hint will be limited to wire.MaxInvPerMsg by the function, so no need to
	// double check it here.
	gdmsg := wire.NewMsgGetDataSizeHint(uint(sm.headerList.Len()))
	numRequested := 0
	var sh *list.Element
	for sh = sm.startHeader; sh != nil; sh = sh.Next() {
		node, ok := sh.Value.(*headerNode)
		if !ok {
			D.Ln("header list node type is not a headerNode")
			continue
		}
		iv := wire.NewInvVect(wire.InvTypeBlock, node.hash)
		haveInv, e := sm.haveInventory(iv)
		if e != nil {
			T.Ln(
				"unexpected failure when checking for existing inventory during header block fetch:",
				e,
			)
		}
		var ee error
		if !haveInv {
			syncPeerState := sm.peerStates[sm.syncPeer]
			sm.requestedBlocks[*node.hash] = struct{}{}
			syncPeerState.requestedBlocks[*node.hash] = struct{}{}
			// If we're fetching from a witness enabled peer post-fork, then ensure that we
			// receive all the witness data in the blocks.
			// if sm.syncPeer.IsWitnessEnabled() {
			// 	iv.Type = wire.InvTypeWitnessBlock
			// }
			ee = gdmsg.AddInvVect(iv)
			if ee != nil {
				D.Ln(ee)
			}
			numRequested++
		}
		sm.startHeader = sh.Next()
		if numRequested >= wire.MaxInvPerMsg {
			break
		}
	}
	if len(gdmsg.InvList) > 0 {
		sm.syncPeer.QueueMessage(gdmsg, nil)
	}
}

// findNextHeaderCheckpoint returns the next checkpoint after the passed height.
// It returns nil when there is not one either because the height is already
// later than the final checkpoint or some other reason such as disabled
// checkpoints.
func (sm *SyncManager) findNextHeaderCheckpoint(height int32) *chaincfg.Checkpoint {
	checkpoints := sm.chain.Checkpoints()
	if len(checkpoints) == 0 {
		return nil
	}
	// There is no next checkpoint if the height is already after the final checkpoint.
	finalCheckpoint := &checkpoints[len(checkpoints)-1]
	if height >= finalCheckpoint.Height {
		return nil
	}
	// Find the next checkpoint.
	nextCheckpoint := finalCheckpoint
	for i := len(checkpoints) - 2; i >= 0; i-- {
		if height >= checkpoints[i].Height {
			break
		}
		nextCheckpoint = &checkpoints[i]
	}
	return nextCheckpoint
}

// handleBlockMsg handles block messages from all peers.
func (sm *SyncManager) handleBlockMsg(workerNumber uint32, bmsg *blockMsg) {
	pp := bmsg.peer
	state, exists := sm.peerStates[pp]
	if !exists {
		T.Ln(
			"received block message from unknown peer", pp,
		)
		return
	}
	// If we didn't ask for this block then the peer is misbehaving.
	blockHash := bmsg.block.Hash()
	if _, exists = state.requestedBlocks[*blockHash]; !exists {
		// The regression test intentionally sends some blocks twice to test duplicate
		// block insertion fails. Don't disconnect the peer or ignore the block when
		// we're in regression test mode in this case so the chain code is actually fed
		// the duplicate blocks.
		if sm.chainParams != &chaincfg.RegressionTestParams {
			W.C(
				func() string {
					return fmt.Sprintf(
						"got unrequested block %v from %s -- disconnecting",
						blockHash,
						pp.Addr(),
					)
				},
			)
			pp.Disconnect()
			return
		}
	}
	// When in headers-first mode, if the block matches the hash of the first header
	// in the list of headers that are being fetched, it's eligible for less
	// validation since the headers have already been verified to link together and
	// are valid up to the next checkpoint. Also, remove the list entry for all
	// blocks except the checkpoint since it is needed to verify the next round of
	// headers links properly.
	isCheckpointBlock := false
	behaviorFlags := blockchain.BFNone
	if sm.headersFirstMode {
		firstNodeEl := sm.headerList.Front()
		if firstNodeEl != nil {
			firstNode := firstNodeEl.Value.(*headerNode)
			if blockHash.IsEqual(firstNode.hash) {
				behaviorFlags |= blockchain.BFFastAdd
				if firstNode.hash.IsEqual(sm.nextCheckpoint.Hash) {
					isCheckpointBlock = true
				} else {
					sm.headerList.Remove(firstNodeEl)
				}
			}
		}
	}
	// Remove block from request maps. Either chain will know about it and so we
	// shouldn't have any more instances of trying to fetch it, or we will fail the
	// insert and thus we'll retry next time we get an inv.
	delete(state.requestedBlocks, *blockHash)
	delete(sm.requestedBlocks, *blockHash)
	var heightUpdate int32
	var blkHashUpdate *chainhash.Hash
	header := &bmsg.block.WireBlock().Header
	if blockchain.ShouldHaveSerializedBlockHeight(header) {
		coinbaseTx := bmsg.block.Transactions()[0]
		cbHeight, e := blockchain.ExtractCoinbaseHeight(coinbaseTx)
		if e != nil {
			T.F(
				"unable to extract height from coinbase tx: %v",
				e,
			)
		} else {
			heightUpdate = cbHeight
			blkHashUpdate = blockHash
		}
	}
	D.Ln("current best height", sm.chain.BestChain.Height())
	_, isOrphan, e := sm.chain.ProcessBlock(
		workerNumber, bmsg.block,
		behaviorFlags, heightUpdate,
	)
	if e != nil {
		if heightUpdate+1 <= sm.chain.BestChain.Height() {
			// Process the block to include validation, best chain selection, orphan handling, etc.
			// When the error is a rule error, it means the block was simply rejected as
			// opposed to something actually going wrong, so log it as such. Otherwise,
			// something really did go wrong, so log it as an actual error.
			// Convert the error into an appropriate reject message and send it.
			if _, ok := e.(blockchain.RuleError); ok {
				E.F(
					"rejected block %v from %s: %v",
					blockHash, pp, e,
				)
			} else {
				E.F("failed to process block %v: %v", blockHash, e)
			}
			if dbErr, ok := e.(database.DBError); ok && dbErr.ErrorCode ==
				database.ErrCorruption {
				panic(dbErr)
			}
			code, reason := mempool.ErrToRejectErr(e)
			pp.PushRejectMsg(wire.CmdBlock, code, reason, blockHash, false)
			return
		} else {
			isOrphan=true
		}
	}
	// Meta-data about the new block this peer is reporting. We use this below to
	// update this peer's lastest block height and the heights of other peers based
	// on their last announced block hash. This allows us to dynamically update the
	// block heights of peers, avoiding stale heights when looking for a new sync
	// peer. Upon acceptance of a block or recognition of an orphan, we also use
	// this information to update the block heights over other peers who's invs may
	// have been ignored if we are actively syncing while the chain is not yet
	// current or who may have lost the lock announcment race. Request the parents
	// for the orphan block from the peer that sent it.
	if isOrphan {
		// We've just received an orphan block from a peer. In order to update the
		// height of the peer, we try to extract the block height from the scriptSig of
		// the coinbase transaction. Extraction is only attempted if the block's version
		// is high enough (ver 2+).
		header := &bmsg.block.WireBlock().Header
		if blockchain.ShouldHaveSerializedBlockHeight(header) {
			coinbaseTx := bmsg.block.Transactions()[0]
			var cbHeight int32
			cbHeight, e = blockchain.ExtractCoinbaseHeight(coinbaseTx)
			if e != nil {
				E.F("unable to extract height from coinbase tx: %v", e)
			} else {
				D.F(
					"extracted height of %v from orphan block",
					cbHeight,
				)
				heightUpdate = cbHeight
				blkHashUpdate = blockHash
			}
		}
		orphanRoot := sm.chain.GetOrphanRoot(blockHash)
		var locator blockchain.BlockLocator
		locator, e = sm.chain.LatestBlockLocator()
		if e != nil {
			E.F(
				"failed to get block locator for the latest block: %v",
				e,
			)
		} else {
			e = pp.PushGetBlocksMsg(locator, orphanRoot)
			if e != nil {
			}
		}
	} else {
		// When the block is not an orphan, log information about it and update the
		// chain state.
		sm.progressLogger.LogBlockHeight(bmsg.block)
		// Update this peer's latest block height, for future potential sync node
		// candidacy.
		best := sm.chain.BestSnapshot()
		heightUpdate = best.Height
		blkHashUpdate = &best.Hash
		// Clear the rejected transactions.
		sm.rejectedTxns = make(map[chainhash.Hash]struct{})
	}
	// Update the block height for this peer. But only send a message to the server
	// for updating peer heights if this is an orphan or our chain is "current".
	// This avoids sending a spammy amount of messages if we're syncing the chain
	// from scratch.
	if blkHashUpdate != nil && heightUpdate != 0 {
		pp.UpdateLastBlockHeight(heightUpdate)
		if isOrphan || sm.current() {
			go sm.peerNotifier.UpdatePeerHeights(
				blkHashUpdate, heightUpdate,
				pp,
			)
		}
	}
	// Nothing more to do if we aren't in headers-first mode.
	if !sm.headersFirstMode {
		return
	}
	// This is headers-first mode, so if the block is not a checkpoint request more
	// blocks using the header list when the request queue is getting short.
	if !isCheckpointBlock {
		if sm.startHeader != nil &&
			len(state.requestedBlocks) < minInFlightBlocks {
			sm.fetchHeaderBlocks()
		}
		return
	}
	// This is headers-first mode and the block is a checkpoint. When there is a
	// next checkpoint, get the next round of headers by asking for headers starting
	// from the block after this one up to the next checkpoint.
	prevHeight := sm.nextCheckpoint.Height
	prevHash := sm.nextCheckpoint.Hash
	sm.nextCheckpoint = sm.findNextHeaderCheckpoint(prevHeight)
	if sm.nextCheckpoint != nil {
		locator := blockchain.BlockLocator([]*chainhash.Hash{prevHash})
		e = pp.PushGetHeadersMsg(locator, sm.nextCheckpoint.Hash)
		if e != nil {
			E.F(
				"failed to send getheaders message to peer %s: %v",
				pp.Addr(), e,
			)
			return
		}
		I.F(
			"downloading headers for blocks %d to %d from peer %s",
			prevHeight+1, sm.nextCheckpoint.Height, sm.syncPeer.Addr(),
		)
		return
	}
	// This is headers-first mode, the block is a checkpoint, and there are no more
	// checkpoints, so switch to normal mode by requesting blocks from the block
	// after this one up to the end of the chain (zero hash).
	sm.headersFirstMode = false
	sm.headerList.Init()
	I.Ln(
		"reached the final checkpoint -- switching to normal mode",
	)
	locator := blockchain.BlockLocator([]*chainhash.Hash{blockHash})
	e = pp.PushGetBlocksMsg(locator, &zeroHash)
	if e != nil {
		E.Ln(
			"failed to send getblocks message to peer", pp, ":", e,
		)
		return
	}
}

// handleBlockchainNotification handles notifications from blockchain. It does
// things such as request orphan block parents and relay accepted blocks to
// connected peers.
func (sm *SyncManager) handleBlockchainNotification(notification *blockchain.Notification) {
	switch notification.Type {
	// A block has been accepted into the block chain. Relay it to other peers.
	case blockchain.NTBlockAccepted:
		// Don't relay if we are not current. Other peers that are current should
		// already know about it.
		if !sm.current() {
			return
		}
		block, ok := notification.Data.(*block2.Block)
		if !ok {
			D.Ln("chain accepted notification is not a block")
			break
		}
		// Generate the inventory vector and relay it.
		iv := wire.NewInvVect(wire.InvTypeBlock, block.Hash())
		sm.peerNotifier.RelayInventory(iv, block.WireBlock().Header)
	// A block has been connected to the main block chain.
	case blockchain.NTBlockConnected:
		block, ok := notification.Data.(*block2.Block)
		if !ok {
			D.Ln("chain connected notification is not a block")
			break
		}
		// Remove all of the transactions (except the coinbase) in the connected block
		// from the transaction pool. Secondly, remove any transactions which are now
		// double spends as a result of these new transactions. Finally, remove any
		// transaction that is no longer an orphan. Transactions which depend on a
		// confirmed transaction are NOT removed recursively because they are still
		// valid.
		for _, tx := range block.Transactions()[1:] {
			sm.txMemPool.RemoveTransaction(tx, false)
			sm.txMemPool.RemoveDoubleSpends(tx)
			sm.txMemPool.RemoveOrphan(tx)
			sm.peerNotifier.TransactionConfirmed(tx)
			acceptedTxs := sm.txMemPool.ProcessOrphans(sm.chain, tx)
			sm.peerNotifier.AnnounceNewTransactions(acceptedTxs)
		}
		// Register block with the fee estimator, if it exists.
		if sm.feeEstimator != nil {
			e := sm.feeEstimator.RegisterBlock(block)
			// If an error is somehow generated then the fee estimator has entered an
			// invalid state. Since it doesn't know how to recover, create a new one.
			if e != nil {
				sm.feeEstimator = mempool.NewFeeEstimator(
					mempool.DefaultEstimateFeeMaxRollback,
					mempool.DefaultEstimateFeeMinRegisteredBlocks,
				)
			}
		}
	// A block has been disconnected from the main block chain.
	case blockchain.NTBlockDisconnected:
		block, ok := notification.Data.(*block2.Block)
		if !ok {
			D.Ln("chain disconnected notification is not a block.")
			break
		}
		// Reinsert all of the transactions (except the coinbase) into the transaction pool.
		for _, tx := range block.Transactions()[1:] {
			var ee error
			_, _, ee = sm.txMemPool.MaybeAcceptTransaction(
				sm.chain, tx,
				false, false,
			)
			if ee != nil {
				// Remove the transaction and all transactions that depend on it if it wasn't
				// accepted into the transaction pool.
				sm.txMemPool.RemoveTransaction(tx, true)
			}
		}
		// Rollback previous block recorded by the fee estimator.
		if sm.feeEstimator != nil {
			e := sm.feeEstimator.Rollback(block.Hash())
			if e != nil {
			}
		}
	}
}

// handleDonePeerMsg deals with peers that have signalled they are done. It
// removes the peer as a candidate for syncing and in the case where it was the
// current sync peer, attempts to select a new best peer to sync from. It is
// invoked from the syncHandler goroutine.
func (sm *SyncManager) handleDonePeerMsg(peer *peerpkg.Peer) {
	state, exists := sm.peerStates[peer]
	if !exists {
		T.Ln("received done peer message for unknown peer", peer)
		return
	}
	// Remove the peer from the list of candidate peers.
	delete(sm.peerStates, peer)
	T.Ln("lost peer ", peer)
	// Remove requested transactions from the global map so that they will be
	// fetched from elsewhere next time we get an inv.
	for txHash := range state.requestedTxns {
		delete(sm.requestedTxns, txHash)
	}
	// Remove requested blocks from the global map so that they will be fetched from
	// elsewhere next time we get an inv.
	//
	// TODO: we could possibly here check which peers have these blocks and request them now to speed things up a little.
	for blockHash := range state.requestedBlocks {
		delete(sm.requestedBlocks, blockHash)
	}
	// Attempt to find a new peer to sync from if the quitting peer is the sync
	// peer. Also, reset the headers-first state if in headers-first mode so
	if sm.syncPeer == peer {
		sm.syncPeer = nil
		if sm.headersFirstMode {
			best := sm.chain.BestSnapshot()
			sm.resetHeaderState(&best.Hash, best.Height)
		}
		sm.startSync()
	}
}

// handleHeadersMsg handles block header messages from all peers. Headers are
// requested when performing a headers-first sync.
func (sm *SyncManager) handleHeadersMsg(hmsg *headersMsg) {
	peer := hmsg.peer
	_, exists := sm.peerStates[peer]
	if !exists {
		T.Ln("received headers message from unknown peer", peer)
		return
	}
	// The remote peer is misbehaving if we didn't request headers.
	msg := hmsg.headers
	numHeaders := len(msg.Headers)
	if !sm.headersFirstMode {
		T.F(
			"got %d unrequested headers from %s -- disconnecting",
			numHeaders, peer,
		)
		peer.Disconnect()
		return
	}
	// Nothing to do for an empty headers message.
	if numHeaders == 0 {
		return
	}
	// Process all of the received headers ensuring each one connects to the
	// previous and that checkpoints match.
	receivedCheckpoint := false
	var finalHash *chainhash.Hash
	for _, blockHeader := range msg.Headers {
		blockHash := blockHeader.BlockHash()
		finalHash = &blockHash
		// Ensure there is a previous header to compare against.
		prevNodeEl := sm.headerList.Back()
		if prevNodeEl == nil {
			W.Ln(
				"header list does not contain a previous element as expected -- disconnecting peer",
			)
			peer.Disconnect()
			return
		}
		// Ensure the header properly connects to the previous one and add it to the
		// list of headers.
		node := headerNode{hash: &blockHash}
		prevNode := prevNodeEl.Value.(*headerNode)
		if prevNode.hash.IsEqual(&blockHeader.PrevBlock) {
			node.height = prevNode.height + 1
			e := sm.headerList.PushBack(&node)
			if sm.startHeader == nil {
				sm.startHeader = e
			}
		} else {
			T.Ln(
				"received block header that does not properly connect to the chain from peer",
				peer,
				"-- disconnecting",
			)
			peer.Disconnect()
			return
		}
		// Verify the header at the next checkpoint height matches.
		if node.height == sm.nextCheckpoint.Height {
			if node.hash.IsEqual(sm.nextCheckpoint.Hash) {
				receivedCheckpoint = true
				I.F(
					"verified downloaded block header against checkpoint at height %d/hash %s",
					node.height,
					node.hash,
				)
			} else {
				T.F(
					"block header at height %d/hash %s from peer %s does NOT match expected checkpoint hash of"+
						" %s -- disconnecting",
					node.height,
					node.hash,
					peer,
					sm.nextCheckpoint.Hash,
				)
				peer.Disconnect()
				return
			}
			break
		}
	}
	// When this header is a checkpoint, switch to fetching the blocks for all of
	// the headers since the last checkpoint.
	if receivedCheckpoint {
		// Since the first entry of the list is always the final block that is already
		// in the database and is only used to ensure the next header links properly, it
		// must be removed before fetching the blocks.
		sm.headerList.Remove(sm.headerList.Front())
		I.F(
			"received %v block headers: Fetching blocks",
			sm.headerList.Len(),
		)
		sm.progressLogger.SetLastLogTime(time.Now())
		sm.fetchHeaderBlocks()
		return
	}
	// This header is not a checkpoint, so request the next batch of headers
	// starting from the latest known header and ending with the next checkpoint.
	locator := blockchain.BlockLocator([]*chainhash.Hash{finalHash})
	e := peer.PushGetHeadersMsg(locator, sm.nextCheckpoint.Hash)
	if e != nil {
		E.F(
			"failed to send getheaders message to peer %s: %v", peer,
			e,
		)
		return
	}
}

// handleInvMsg handles inv messages from all peers. We examine the inventory
// advertised by the remote peer and act accordingly.
func (sm *SyncManager) handleInvMsg(imsg *invMsg) {
	peer := imsg.peer
	state, exists := sm.peerStates[peer]
	if !exists {
		T.Ln("received inv message from unknown peer", peer)
		return
	}
	// Attempt to find the final block in the inventory list.  There may not be one.
	lastBlock := -1
	invVects := imsg.inv.InvList
	for i := len(invVects) - 1; i >= 0; i-- {
		if invVects[i].Type == wire.InvTypeBlock {
			lastBlock = i
			break
		}
	}
	// If this inv contains a block announcement, and this isn't coming from our
	// current sync peer or we're current, then update the last announced block for
	// this peer. We'll use this information later to update the heights of peers
	// based on blocks we've accepted that they previously announced.
	if lastBlock != -1 && (peer != sm.syncPeer || sm.current()) {
		peer.UpdateLastAnnouncedBlock(&invVects[lastBlock].Hash)
	}
	// Ignore invs from peers that aren't the sync if we are not current. Helps prevent fetching a mass of orphans.
	if peer != sm.syncPeer && !sm.current() {
		return
	}
	// If our chain is current and a peer announces a block we already know of, then update their current block height.
	if lastBlock != -1 && sm.current() {
		blkHeight, e := sm.chain.BlockHeightByHash(&invVects[lastBlock].Hash)
		if e == nil {
			peer.UpdateLastBlockHeight(blkHeight)
		}
	}
	// Request the advertised inventory if we don't already have it. Also, request
	// parent blocks of orphans if we receive one we already have. Finally, attempt
	// to detect potential stalls due to long side chains we already have and
	// request more blocks to prevent them.
	for i, iv := range invVects {
		// Ignore unsupported inventory types.
		switch iv.Type {
		case wire.InvTypeBlock:
		case wire.InvTypeTx:
		// case wire.InvTypeWitnessBlock:
		// case wire.InvTypeWitnessTx:
		default:
			continue
		}
		// Add the inventory to the cache of known inventory for the peer.
		peer.AddKnownInventory(iv)
		// Ignore inventory when we're in headers-first mode.
		if sm.headersFirstMode {
			continue
		}
		// Request the inventory if we don't already have it.
		haveInv, e := sm.haveInventory(iv)
		if e != nil {
			E.Ln("unexpected failure when checking for existing inventory during inv message processing:", e)
			continue
		}
		if !haveInv {
			if iv.Type == wire.InvTypeTx {
				// Skip the transaction if it has already been rejected.
				if _, exists := sm.rejectedTxns[iv.Hash]; exists {
					continue
				}
			}
			// Ignore invs block invs from non-witness enabled peers, as after segwit
			// activation we only want to download from peers that can provide us full
			// witness data for blocks. PARALLELCOIN HAS NO WITNESS STUFF if
			// !peer.IsWitnessEnabled() && iv.Type == wire.InvTypeBlock {
			// 	continue
			// }
			// Add it to the request queue.
			state.requestQueue = append(state.requestQueue, iv)
			continue
		}
		if iv.Type == wire.InvTypeBlock {
			// The block is an orphan block that we already have. When the existing orphan
			// was processed, it requested the missing parent blocks. When this scenario
			// happens, it means there were more blocks missing than are allowed into a
			// single inventory message. As a result, once this peer requested the final
			// advertised block, the remote peer noticed and is now resending the orphan
			// block as an available block to signal there are more missing blocks that need
			// to be requested.
			if sm.chain.IsKnownOrphan(&iv.Hash) {
				// Request blocks starting at the latest known up to the root of the orphan that just came in.
				orphanRoot := sm.chain.GetOrphanRoot(&iv.Hash)
				locator, e := sm.chain.LatestBlockLocator()
				if e != nil {
					E.Ln("failed to get block locator for the latest block:", e)
					continue
				}
				e = peer.PushGetBlocksMsg(locator, orphanRoot)
				if e != nil {
				}
				continue
			}
			// We already have the final block advertised by this inventory message, so
			// force a request for more. This should only happen if we're on a really long
			// side chain.
			if i == lastBlock {
				// Request blocks after this one up to the final one the remote peer knows about
				// (zero stop hash).
				locator := sm.chain.BlockLocatorFromHash(&iv.Hash)
				e := peer.PushGetBlocksMsg(locator, &zeroHash)
				if e != nil {
				}
			}
		}
	}
	// Request as much as possible at once. Anything that won't fit into request
	// will be requested on the next inv message.
	numRequested := 0
	gdmsg := wire.NewMsgGetData()
	requestQueue := state.requestQueue
	for len(requestQueue) != 0 {
		iv := requestQueue[0]
		requestQueue[0] = nil
		requestQueue = requestQueue[1:]
		switch iv.Type {
		// case wire.InvTypeWitnessBlock:
		// 	fallthrough
		case wire.InvTypeBlock:
			// Request the block if there is not already a pending request.
			if _, exists := sm.requestedBlocks[iv.Hash]; !exists {
				sm.requestedBlocks[iv.Hash] = struct{}{}
				sm.limitMap(sm.requestedBlocks, maxRequestedBlocks)
				state.requestedBlocks[iv.Hash] = struct{}{}
				// if peer.IsWitnessEnabled() {
				// 	iv.Type = wire.InvTypeWitnessBlock
				// }
				e := gdmsg.AddInvVect(iv)
				if e != nil {
				}
				numRequested++
			}
		// case wire.InvTypeWitnessTx:
		// 	fallthrough
		case wire.InvTypeTx:
			// Request the transaction if there is not already a pending request.
			if _, exists := sm.requestedTxns[iv.Hash]; !exists {
				sm.requestedTxns[iv.Hash] = struct{}{}
				sm.limitMap(sm.requestedTxns, maxRequestedTxns)
				state.requestedTxns[iv.Hash] = struct{}{}
				// If the peer is capable, request the txn including all witness
				// data.
				// if peer.IsWitnessEnabled() {
				// 	iv.Type = wire.InvTypeWitnessTx
				// }
				e := gdmsg.AddInvVect(iv)
				if e != nil {
				}
				numRequested++
			}
		}
		if numRequested >= wire.MaxInvPerMsg {
			break
		}
	}
	state.requestQueue = requestQueue
	if len(gdmsg.InvList) > 0 {
		peer.QueueMessage(gdmsg, nil)
	}
}

// handleNewPeerMsg deals with new peers that have signalled they may be
// considered as a sync peer (they have already successfully negotiated). It
// also starts syncing if needed. It is invoked from the syncHandler goroutine.
func (sm *SyncManager) handleNewPeerMsg(peer *peerpkg.Peer) {
	// Ignore if in the process of shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}
	T.F("new valid peer %s (%s)", peer, peer.UserAgent())
	// Initialize the peer state
	isSyncCandidate := sm.isSyncCandidate(peer)
	if isSyncCandidate {
		I.Ln(peer, "is a sync candidate")
	}
	sm.peerStates[peer] = &peerSyncState{
		syncCandidate:   isSyncCandidate,
		requestedTxns:   make(map[chainhash.Hash]struct{}),
		requestedBlocks: make(map[chainhash.Hash]struct{}),
	}
	// Start syncing by choosing the best candidate if needed.
	if isSyncCandidate && sm.syncPeer == nil {
		sm.startSync()
	}
}

// handleTxMsg handles transaction messages from all peers.
func (sm *SyncManager) handleTxMsg(tmsg *txMsg) {
	peer := tmsg.peer
	state, exists := sm.peerStates[peer]
	if !exists {
		W.C(
			func() string {
				return "received tx message from unknown peer " +
					peer.String()
			},
		)
		return
	}
	// NOTE: BitcoinJ, and possibly other wallets, don't follow the spec of sending
	// an inventory message and allowing the remote peer to decide whether or not
	// they want to request the transaction via a getdata message. Unfortunately,
	// the reference implementation permits unrequested data, so it has allowed
	// wallets that don't follow the spec to proliferate. While this is not ideal,
	// there is no check here to disconnect peers for sending unsolicited
	// transactions to provide interoperability.
	txHash := tmsg.tx.Hash()
	// Ignore transactions that we have already rejected. Do not send a reject
	// message here because if the transaction was already rejected, the transaction
	// was unsolicited.
	if _, exists = sm.rejectedTxns[*txHash]; exists {
		D.C(
			func() string {
				return "ignoring unsolicited previously rejected transaction " +
					txHash.String() + " from " + peer.String()
			},
		)
		return
	}
	// Process the transaction to include validation, insertion in the memory pool,
	// orphan handling, etc.
	acceptedTxs, e := sm.txMemPool.ProcessTransaction(
		sm.chain, tmsg.tx,
		true, true, mempool.Tag(peer.ID()),
	)
	// Remove transaction from request maps. Either the mempool/chain already knows
	// about it and as such we shouldn't have any more instances of trying to fetch
	// it, or we failed to insert and thus we'll retry next time we get an inv.
	delete(state.requestedTxns, *txHash)
	delete(sm.requestedTxns, *txHash)
	if e != nil {
		// Do not request this transaction again until a new block has been processed.
		sm.rejectedTxns[*txHash] = struct{}{}
		sm.limitMap(sm.rejectedTxns, maxRejectedTxns)
		// When the error is a rule error, it means the transaction was simply rejected
		// as opposed to something actually going wrong, so log it as such. Otherwise,
		// something really did go wrong, so log it as an actual error.
		if _, ok := e.(mempool.RuleError); ok {
			D.F(
				"rejected transaction %v from %s: %v",
				txHash,
				peer,
				e,
			)
		} else {
			E.F(
				"failed to process transaction %v: %v",
				txHash,
				e,
			)
		}
		// Convert the error into an appropriate reject message and send it.
		code, reason := mempool.ErrToRejectErr(e)
		peer.PushRejectMsg(wire.CmdTx, code, reason, txHash, false)
		return
	}
	sm.peerNotifier.AnnounceNewTransactions(acceptedTxs)
}

// haveInventory returns whether or not the inventory represented by the passed
// inventory vector is known. This includes checking all of the various places
// inventory can be when it is in different states such as blocks that are part
// of the main chain, on a side chain, in the orphan pool, and transactions that
// are in the memory pool (either the main pool or orphan pool).
func (sm *SyncManager) haveInventory(invVect *wire.InvVect) (bool, error) {
	switch invVect.Type {
	// case wire.InvTypeWitnessBlock:
	// 	fallthrough
	case wire.InvTypeBlock:
		// Ask chain if the block is known to it in any form (main chain, side chain, or
		// orphan).
		return sm.chain.HaveBlock(&invVect.Hash)
	// case wire.InvTypeWitnessTx:
	// 	fallthrough
	case wire.InvTypeTx:
		// Ask the transaction memory pool if the transaction is known to it in any form
		// (main pool or orphan).
		if sm.txMemPool.HaveTransaction(&invVect.Hash) {
			return true, nil
		}
		// Chk if the transaction exists from the point of view of the end of the main
		// chain. Note that this is only a best effort since it is expensive to check
		// existence of every output and the only purpose of this check is to avoid
		// downloading already known transactions. Only the first two outputs are
		// checked because the vast majority of transactions consist of two outputs
		// where one is some form of "pay-to-somebody-else" and the other is a change
		// output.
		prevOut := wire.OutPoint{Hash: invVect.Hash}
		for i := uint32(0); i < 2; i++ {
			prevOut.Index = i
			entry, e := sm.chain.FetchUtxoEntry(prevOut)
			if e != nil {
				return false, e
			}
			if entry != nil && !entry.IsSpent() {
				return true, nil
			}
		}
		return false, nil
	}
	// The requested inventory is is an unsupported type, so just claim it is known
	// to avoid requesting it.
	return true, nil
}

// isSyncCandidate returns whether or not the peer is a candidate to consider
// syncing from.
func (sm *SyncManager) isSyncCandidate(peer *peerpkg.Peer) bool {
	// Typically a peer is not a candidate for sync if it's not a full node, however
	// regression test is special in that the regression tool is not a full node and
	// still needs to be considered a sync candidate.
	if sm.chainParams == &chaincfg.RegressionTestParams {
		// The peer is not a candidate if it's not coming from localhost or the hostname
		// can't be determined for some reason.
		var host string
		var e error
		host, _, e = net.SplitHostPort(peer.Addr())
		if e != nil {
			return false
		}
		if host != "127.0.0.1" && host != "localhost" {
			return false
		}
		// } else {
		// // The peer is not a candidate for sync if it's not a full node. Additionally, if the segwit soft-fork package
		// // has activated, then the peer must also be upgraded.
		// segwitActive, e := sm.chain.IsDeploymentActive(chaincfg.DeploymentSegwit)
		// if e != nil  {
		// 			// 	Error("unable to query for segwit soft-fork state:", e)
		// }
		// nodeServices := peer.Services()
		// if nodeServices&wire.SFNodeNetwork != wire.SFNodeNetwork ||
		// 	(segwitActive && !peer.IsWitnessEnabled()) {
		// 	return false
		// }
	}
	// Candidate if all checks passed.
	return true
}

// limitMap is a helper function for maps that require a maximum limit by
// evicting a random transaction if adding a new value would cause it to
// overflow the maximum allowed.
func (sm *SyncManager) limitMap(m map[chainhash.Hash]struct{}, limit int) {
	if len(m)+1 > limit {
		// Remove a random entry from the map. For most compilers, Go's range statement
		// iterates starting at a random item although that is not 100% guaranteed by
		// the spec. The iteration order is not important here because an adversary
		// would have to be able to pull off preimage attacks on the hashing function in
		// order to target eviction of specific entries anyways.
		for txHash := range m {
			delete(m, txHash)
			return
		}
	}
}

// resetHeaderState sets the headers-first mode state to values appropriate for
// syncing from a new peer.
func (sm *SyncManager) resetHeaderState(newestHash *chainhash.Hash, newestHeight int32) {
	sm.headersFirstMode = false
	sm.headerList.Init()
	sm.startHeader = nil
	// When there is a next checkpoint, add an entry for the latest known block into
	// the header pool. This allows the next downloaded header to prove it links to
	// the chain properly.
	if sm.nextCheckpoint != nil {
		node := headerNode{height: newestHeight, hash: newestHash}
		sm.headerList.PushBack(&node)
	}
}

// startSync will choose the best peer among the available candidate peers to
// download/sync the blockchain from. When syncing is already running, it simply
// returns. It also examines the candidates for any which are no longer
// candidates and removes them as needed.
func (sm *SyncManager) startSync() {
	// Return now if we're already syncing.
	if sm.syncPeer != nil {
		return
	}
	// Once the segwit soft-fork package has activated, we only want to sync from
	// peers which are witness enabled to ensure that we fully validate all
	// blockchain data.
	// var e error
	// var segwitActive bool
	// segwitActive, e = sm.chain.IsDeploymentActive(chaincfg.DeploymentSegwit)
	// if e != nil  {
	// 	Error("unable to query for segwit soft-fork state:", e)
	// 	return
	// }
	best := sm.chain.BestSnapshot()
	var bestPeer *peerpkg.Peer
	for peer, state := range sm.peerStates {
		if !state.syncCandidate {
			continue
		}
		// if segwitActive && !peer.IsWitnessEnabled() {
		// 	D.Ln("peer", peer, "not witness enabled, skipping")
		// 	continue
		// } Remove sync candidate peers that are no longer candidates due to passing
		// their latest known block.
		//
		// NOTE: The < is intentional as opposed to <=. While technically the peer
		// doesn't have a later block when it's equal, it will likely have one soon so
		// it is a reasonable choice. It also allows the case where both are at 0 such
		// as during regression test.
		if peer.LastBlock() < best.Height {
			// state.syncCandidate = false
			continue
		}
		// TODO(davec): Use a better algorithm to choose the best peer. For now, just pick the first available candidate.
		bestPeer = peer
	}
	// Start syncing from the best peer if one was selected.
	if bestPeer != nil {
		// Clear the requestedBlocks if the sync peer changes, otherwise we may ignore blocks we need that the last sync
		// peer failed to send.
		sm.requestedBlocks = make(map[chainhash.Hash]struct{})
		locator, e := sm.chain.LatestBlockLocator()
		if e != nil {
			E.Ln("failed to get block locator for the latest block:", e)
			return
		}
		T.C(
			func() string {
				return fmt.Sprintf("syncing to block height %d from peer %v", bestPeer.LastBlock(), bestPeer.Addr())
			},
		)
		// When the current height is less than a known checkpoint we can use block
		// headers to learn about which blocks comprise the chain up to the checkpoint
		// and perform less validation for them. This is possible since each header
		// contains the hash of the previous header and a merkle root.
		//
		// Therefore if we validate all of the received headers link together properly
		// and the checkpoint hashes match, we can be sure the hashes for the blocks in
		// between are accurate. Further, once the full blocks are downloaded, the
		// merkle root is computed and compared against the value in the header which
		// proves the full block hasn't been tampered with. Once we have passed the
		// final checkpoint, or checkpoints are disabled, use standard inv messages
		// learn about the blocks and fully validate them. Finally, regression test mode
		// does not support the headers-first approach so do normal block downloads when
		// in regression test mode.
		if sm.nextCheckpoint != nil &&
			best.Height < sm.nextCheckpoint.Height &&
			sm.chainParams != &chaincfg.RegressionTestParams {
			e := bestPeer.PushGetHeadersMsg(locator, sm.nextCheckpoint.Hash)
			if e != nil {
			}
			sm.headersFirstMode = true
			I.F(
				"downloading headers for blocks %d to %d from peer %s",
				best.Height+1,
				sm.nextCheckpoint.Height,
				bestPeer.Addr(),
			)
		} else {
			e := bestPeer.PushGetBlocksMsg(locator, &zeroHash)
			if e != nil {
			}
		}
		sm.syncPeer = bestPeer
	} else {
		T.Ln("no sync peer candidates available")
	}
}

// New constructs a new SyncManager. Use Start to begin processing asynchronous
// block, tx, and inv updates.
func New(config *Config) (*SyncManager, error) {
	sm := SyncManager{
		peerNotifier:    config.PeerNotifier,
		chain:           config.Chain,
		txMemPool:       config.TxMemPool,
		chainParams:     config.ChainParams,
		rejectedTxns:    make(map[chainhash.Hash]struct{}),
		requestedTxns:   make(map[chainhash.Hash]struct{}),
		requestedBlocks: make(map[chainhash.Hash]struct{}),
		peerStates:      make(map[*peerpkg.Peer]*peerSyncState),
		progressLogger:  newBlockProgressLogger("processed"),
		msgChan:         make(chan interface{}, config.MaxPeers*3),
		headerList:      list.New(),
		quit:            qu.T(),
		feeEstimator:    config.FeeEstimator,
	}
	best := sm.chain.BestSnapshot()
	if !config.DisableCheckpoints {
		// Initialize the next checkpoint based on the current height.
		sm.nextCheckpoint = sm.findNextHeaderCheckpoint(best.Height)
		if sm.nextCheckpoint != nil {
			sm.resetHeaderState(&best.Hash, best.Height)
		}
	} else {
		I.Ln("checkpoints are disabled")
	}
	sm.chain.Subscribe(sm.handleBlockchainNotification)
	return &sm, nil
}
