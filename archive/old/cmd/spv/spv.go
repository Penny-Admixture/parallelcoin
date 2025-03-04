package spv

import (
	"errors"
	"fmt"
	"github.com/p9c/pod/pkg/amt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	
	"github.com/p9c/pod/pkg/util/qu"
	
	"github.com/p9c/pod/cmd/spv/cache/lru"
	"github.com/p9c/pod/cmd/spv/filterdb"
	"github.com/p9c/pod/cmd/spv/headerfs"
	"github.com/p9c/pod/pkg/addrmgr"
	"github.com/p9c/pod/pkg/blockchain"
	"github.com/p9c/pod/pkg/chaincfg"
	"github.com/p9c/pod/pkg/chainhash"
	"github.com/p9c/pod/pkg/connmgr"
	"github.com/p9c/pod/pkg/peer"
	"github.com/p9c/pod/pkg/waddrmgr"
	"github.com/p9c/pod/pkg/walletdb"
	"github.com/p9c/pod/pkg/wire"
)

type (
	// ChainService is instantiated with functional options
	ChainService struct {
		// The following variables must only be used atomically. Putting the uint64s first makes them 64-bit aligned for
		// 32-bit systems.
		bytesReceived    uint64 // Total bytes received from all peers since start.
		bytesSent        uint64 // Total bytes sent by all peers since start.
		started          int32
		shutdown         int32
		FilterDB         filterdb.FilterDatabase
		BlockHeaders     headerfs.BlockHeaderStore
		RegFilterHeaders *headerfs.FilterHeaderStore
		FilterCache      *lru.Cache
		BlockCache       *lru.Cache
		// queryPeers will be called to send messages to one or more peers, expecting a response.
		queryPeers func(
			wire.Message, func(
			*ServerPeer, wire.Message,
			chan<- struct{},
		), ...QueryOption,
		)
		// queryBatch will be called to distribute a batch of messages across our connected peers.
		queryBatch func(
			[]wire.Message, func(
			*ServerPeer, wire.Message,
			wire.Message,
		) bool, qu.C, ...QueryOption,
		)
		chainParams       chaincfg.Params
		addrManager       *addrmgr.AddrManager
		connManager       *connmgr.ConnManager
		blockManager      *blockManager
		newPeers          chan *ServerPeer
		donePeers         chan *ServerPeer
		banPeers          chan *ServerPeer
		query             chan interface{}
		peerHeightsUpdate chan updatePeerHeightsMsg
		wg                sync.WaitGroup
		quit              qu.C
		timeSource        blockchain.MedianTimeSource
		services          wire.ServiceFlag
		blockSubscribers  map[*blockSubscription]struct{}
		mtxSubscribers    sync.RWMutex
		utxoScanner       *UtxoScanner
		// TODO: Add a map for more granular exclusion?
		mtxCFilter sync.Mutex
		// These are only necessary until the block subscription logic is refactored out into its own package and we can
		// have different message types sent in the notifications.
		//
		// TODO(aakselrod): Get rid of this when doing the refactoring above.
		reorgedBlockHeaders map[chainhash.Hash]*wire.BlockHeader
		mtxReorgHeader      sync.RWMutex
		userAgentName       string
		userAgentVersion    string
		nameResolver        func(string) ([]net.IP, error)
		dialer              func(net.Addr) (net.Conn, error)
	}
	// Config is a struct detailing the configuration of the chain service.
	Config struct {
		// DataDir is the directory that neutrino will store all header information within.
		DataDir string
		// Database is an *open* database instance that we'll use to storm indexes of teh chain.
		Database walletdb.DB
		// ChainParams is the chain that we're running on.
		ChainParams chaincfg.Params
		// ConnectPeers is a slice of hosts that should be connected to on startup, and be established as persistent
		// peers.
		//
		// NOTE: If specified, we'll *only* connect to this set of peers and won't attempt to automatically seek
		// outbound peers.
		ConnectPeers []string
		// AddPeers is a slice of hosts that should be connected to on startup, and be maintained as persistent peers.
		AddPeers []string
		// Dialer is an optional function closure that will be used to establish outbound TCP connections. If specified,
		// then the connection manager will use this in place of net.Dial for all outbound connection attempts.
		Dialer func(addr net.Addr) (net.Conn, error)
		// NameResolver is an optional function closure that will be used to lookup the IP of any host. If specified,
		// then the address manager, along with regular outbound connection attempts will use this instead.
		NameResolver func(host string) ([]net.IP, error)
		// FilterCacheSize indicates the size (in bytes) of filters the cache will hold in memory at most.
		FilterCacheSize uint64
		// BlockCacheSize indicates the size (in bytes) of blocks the block cache will hold in memory at most.
		BlockCacheSize uint64
	}
	// ServerPeer extends the peer to maintain state shared by the server and the blockmanager.
	ServerPeer struct {
		// The following variables must only be used atomically
		feeFilter int64
		*peer.Peer
		connReq    *connmgr.ConnReq
		server     *ChainService
		persistent bool
		// continueHash   *chainhash.Hash
		// requestQueue   []*wire.InvVect
		knownAddresses map[string]struct{}
		banScore       connmgr.DynamicBanScore
		quit           qu.C
		// The following map of subcribers is used to subscribe to messages from the peer. This allows broadcast to
		// multiple subscribers at once, allowing for multiple queries to be going to multiple peers at any one time.
		// The mutex is for subscribe/unsubscribe functionality. The sends on these channels WILL NOT block; any
		// messages the channel can't accept will be dropped silently.
		recvSubscribers map[spMsgSubscription]struct{}
		mtxSubscribers  sync.RWMutex
	}
	// peerState maintains state of inbound, persistent, outbound peers as well as banned peers and outbound groups.
	peerState struct {
		outboundPeers   map[int32]*ServerPeer
		persistentPeers map[int32]*ServerPeer
		banned          map[string]time.Time
		outboundGroups  map[string]int
	}
	// spMsg represents a message over the wire from a specific peer.
	spMsg struct {
		sp  *ServerPeer
		msg wire.Message
	}
	// spMsgSubscription sends all messages from a peer over a channel, allowing pluggable filtering of the messages.
	spMsgSubscription struct {
		msgChan  chan<- spMsg
		quitChan qu.C
	}
	// updatePeerHeightsMsg is a message sent from the blockmanager to the server after a new block has been accepted.
	// The purpose of the message is to update the heights of peers that were known to announce the block before we
	// connected it to the main chain or recognized it as an orphan. With these updates, peer heights will be kept up to
	// date, allowing for fresh data when selecting sync peer candidacy.
	updatePeerHeightsMsg struct {
		newHash    *chainhash.Hash
		newHeight  int32
		originPeer *ServerPeer
	}
)

// These are exported variables so they can be changed by users.
//
// TODO: Export functional options for these as much as possible so they can be changed call-to-call.
var (
	// BanDuration is the duration of a ban.
	BanDuration = time.Hour * 24
	// BanThreshold is the maximum ban score before a peer is banned.
	BanThreshold = uint32(100)
	// ConnectionRetryInterval is the base amount of time to wait in between retries when connecting to persistent
	// peers. It is adjusted by the number of retries such that there is a retry backoff.
	ConnectionRetryInterval = time.Second * 60
	// DefaultBlockCacheSize is the size (in bytes) of blocks neutrino will keep in memory if no size is specified in
	// the neutrino.Config.
	DefaultBlockCacheSize uint64 = 4096 * 10 * 1000 // 40 MB
	// DefaultFilterCacheSize is the size (in bytes) of filters neutrino will keep in memory if no size is specified in
	// the neutrino.Config.
	DefaultFilterCacheSize uint64 = 4096 * 1000
	// DisableDNSSeed disables getting initial addresses for Bitcoin nodes from DNS.
	DisableDNSSeed = false
	// MaxPeers is the maximum number of connections the client maintains.
	MaxPeers = 125
	// RequiredServices describes the services that are required to be supported by outbound peers.
	RequiredServices = wire.SFNodeNetwork | /* wire.SFNodeWitness |*/ wire.SFNodeCF
	// Services describes the services that are supported by the server.
	Services = /*wire.SFNodeWitness |*/ wire.SFNodeCF
	// TargetOutbound is the number of outbound peers to target.
	TargetOutbound = 16
	// UserAgentName is the user agent name and is used to help identify ourselves to other bitcoin peers.
	UserAgentName = "neutrino"
	// UserAgentVersion is the user agent version and is used to help identify ourselves to other bitcoin peers.
	UserAgentVersion = "0.0.4-beta"
)

// AddBytesReceived adds the passed number of bytes to the total bytes received counter for the server. It is safe for
// concurrent access.
func (s *ChainService) AddBytesReceived(bytesReceived uint64) {
	atomic.AddUint64(&s.bytesReceived, bytesReceived)
}

// AddBytesSent adds the passed number of bytes to the total bytes sent counter for the server. It is safe for
// concurrent access.
func (s *ChainService) AddBytesSent(bytesSent uint64) {
	atomic.AddUint64(&s.bytesSent, bytesSent)
}

// AddPeer adds a new peer that has already been connected to the server.
func (s *ChainService) AddPeer(sp *ServerPeer) {
	s.newPeers <- sp
}

// BanPeer bans a peer that has already been connected to the server by ip.
func (s *ChainService) BanPeer(sp *ServerPeer) {
	s.banPeers <- sp
}

// BestBlock retrieves the most recent block's height and hash where we have both the header and filter header ready.
func (s *ChainService) BestBlock() (*waddrmgr.BlockStamp, error) {
	bestHeader, bestHeight, e := s.BlockHeaders.ChainTip()
	if e != nil {
		return nil, e
	}
	_, filterHeight, e := s.RegFilterHeaders.ChainTip()
	if e != nil {
		return nil, e
	}
	// Filter headers might lag behind block headers, so we can can fetch a previous block header if the filter headers
	// are not caught up.
	if filterHeight < bestHeight {
		bestHeight = filterHeight
		bestHeader, e = s.BlockHeaders.FetchHeaderByHeight(bestHeight)
		if e != nil {
			return nil, e
		}
	}
	return &waddrmgr.BlockStamp{
			Height: int32(bestHeight),
			Hash:   bestHeader.BlockHash(),
		},
		nil
}

// ChainParams returns a copy of the ChainService's chaincfg.Params.
func (s *ChainService) ChainParams() chaincfg.Params {
	return s.chainParams
}

// GetBlockHash returns the block hash at the given height.
func (s *ChainService) GetBlockHash(height int64) (*chainhash.Hash, error) {
	header, e := s.BlockHeaders.FetchHeaderByHeight(uint32(height))
	if e != nil {
		return nil, e
	}
	hash := header.BlockHash()
	return &hash, e
}

// GetBlockHeader returns the block header for the given block hash, or an error if the hash doesn't exist or is
// unknown.
func (s *ChainService) GetBlockHeader(
	blockHash *chainhash.Hash,
) (header *wire.BlockHeader, e error) {
	header, _, e = s.BlockHeaders.FetchHeader(blockHash)
	return header, e
}

// GetBlockHeight gets the height of a block by its hash. An error is returned if the given block hash is unknown.
func (s *ChainService) GetBlockHeight(hash *chainhash.Hash) (int32, error) {
	_, height, e := s.BlockHeaders.FetchHeader(hash)
	if e != nil {
		return 0, e
	}
	return int32(height), nil
}

// IsCurrent lets the caller know whether the chain service's block manager thinks its view of the network is current.
func (s *ChainService) IsCurrent() bool {
	return s.blockManager.IsFullySynced()
}

// NetTotals returns the sum of all bytes received and sent across the network for all peers. It is safe for concurrent
// access.
func (s *ChainService) NetTotals() (uint64, uint64) {
	return atomic.LoadUint64(&s.bytesReceived),
		atomic.LoadUint64(&s.bytesSent)
}

// PeerByAddr lets the caller look up a peer address in the service's peer table, if connected to that peer address.
func (s *ChainService) PeerByAddr(addr string) *ServerPeer {
	for _, serverPeer := range s.Peers() {
		if serverPeer.Addr() == addr {
			return serverPeer
		}
	}
	return nil
}

// PublishTransaction sends the transaction to the consensus RPC server so it can be propigated to other nodes and
// eventually mined.
func (s *ChainService) PublishTransaction(tx *wire.MsgTx) (e error) {
	// TODO(roasbeef): pipe through querying interface
	/*_, e := s.rpcClient.SendRawTransaction(tx, false)
	return e*/
	return nil
}

// Start begins connecting to peers and syncing the blockchain.
func (s *ChainService) Start() {
	// Already started?
	if atomic.AddInt32(&s.started, 1) != 1 {
		return
	}
	// Start the peer handler which in turn starts the address and block managers.
	s.wg.Add(1)
	go s.peerHandler()
}

// Stop gracefully shuts down the server by stopping and disconnecting all peers and the main listener.
func (s *ChainService) Stop() (e error) {
	// Make sure this only happens once.
	if atomic.AddInt32(&s.shutdown, 1) != 1 {
		return nil
	}
	// Signal the remaining goroutines to quit.
	s.quit.Q()
	s.wg.Wait()
	return nil
}

// UpdatePeerHeights updates the heights of all peers who have have announced the latest connected main chain block, or
// a recognized orphan. These height updates allow us to dynamically refresh peer heights, ensuring sync peer selection
// has access to the latest block heights for each peer.
func (s *ChainService) UpdatePeerHeights(latestBlkHash *chainhash.Hash, latestHeight int32, updateSource *ServerPeer) {
	s.peerHeightsUpdate <- updatePeerHeightsMsg{
		newHash:    latestBlkHash,
		newHeight:  latestHeight,
		originPeer: updateSource,
	}
}

// addrStringToNetAddr takes an address in the form of 'host:port' or 'host' and returns a net.Addr which maps to the
// original address with any host names resolved to IP addresses and a default port added, if not specified, from the
// ChainService's network parameters.
func (s *ChainService) addrStringToNetAddr(addr string) (net.Addr, error) {
	host, strPort, e := net.SplitHostPort(addr)
	if e != nil {
		switch e.(type) {
		case *net.AddrError:
			host = addr
			strPort = s.ChainParams().DefaultPort
		default:
			return nil, e
		}
	}
	// Attempt to look up an IP address associated with the parsed host.
	ips, e := s.nameResolver(host)
	if e != nil {
		return nil, e
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses found for %s", host)
	}
	port, e := strconv.Atoi(strPort)
	if e != nil {
		return nil, e
	}
	return &net.TCPAddr{
			IP:   ips[0],
			Port: port,
		},
		nil
}

// handleAddPeerMsg deals with adding new peers. It is invoked from the peerHandler goroutine.
func (s *ChainService) handleAddPeerMsg(state *peerState, sp *ServerPeer) bool {
	if sp == nil {
		return false
	}
	// Ignore new peers if we're shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		I.F("new peer %s ignored - server is shutting down", sp)
		sp.Disconnect()
		return false
	}
	// Disconnect banned peers.
	var host string
	var e error
	host, _, e = net.SplitHostPort(sp.Addr())
	if e != nil {
		D.Ln("can't split host/port:", e)
		sp.Disconnect()
		return false
	}
	if banEnd, ok := state.banned[host]; ok {
		if time.Now().Before(banEnd) {
			D.F(
				"peer %s is banned for another %v - disconnecting %s",
				host, time.Until(banEnd),
			)
			sp.Disconnect()
			return false
		}
		I.F(
			"peer %s is no longer banned", host,
		)
		delete(state.banned, host)
	}
	// TODO: Chk for max peers from a single IP. Limit max number of total peers.
	if state.Count() >= MaxPeers {
		I.F(
			"max peers reached [%d] - disconnecting peer %s",
			MaxPeers, sp,
		)
		sp.Disconnect()
		// TODO: how to handle permanent peers here? they should be rescheduled.
		return false
	}
	// Add the new peer and start it.
	D.Ln("new peer", sp)
	state.outboundGroups[addrmgr.GroupKey(sp.NA())]++
	if sp.persistent {
		state.persistentPeers[sp.ID()] = sp
	} else {
		state.outboundPeers[sp.ID()] = sp
	}
	return true
}

// handleBanPeerMsg deals with banning peers. It is invoked from the peerHandler goroutine.
func (s *ChainService) handleBanPeerMsg(state *peerState, sp *ServerPeer) {
	var host string
	var e error
	host, _, e = net.SplitHostPort(sp.Addr())
	if e != nil {
		D.F("can't split ban peer %s: %s %s", sp.Addr(), e)
		return
	}
	I.F("banned peer %s for %v", host, BanDuration)
	state.banned[host] = time.Now().Add(BanDuration)
}

// handleDonePeerMsg deals with peers that have signalled they are done. It is invoked from the peerHandler goroutine.
func (s *ChainService) handleDonePeerMsg(state *peerState, sp *ServerPeer) {
	var list map[int32]*ServerPeer
	if sp.persistent {
		list = state.persistentPeers
	} else {
		list = state.outboundPeers
	}
	if _, ok := list[sp.ID()]; ok {
		if !sp.Inbound() && sp.VersionKnown() {
			state.outboundGroups[addrmgr.GroupKey(sp.NA())]--
		}
		if !sp.Inbound() && sp.connReq != nil {
			s.connManager.Disconnect(sp.connReq.ID())
		}
		delete(list, sp.ID())
		D.Ln("removed peer", sp)
		return
	}
	if sp.connReq != nil {
		s.connManager.Disconnect(sp.connReq.ID())
	}
	// Update the address' last seen time if the peer has acknowledged our version and has sent us its version as well.
	if sp.VerAckReceived() && sp.VersionKnown() && sp.NA() != nil {
		s.addrManager.Connected(sp.NA())
	}
	// If we get here it means that either we didn't know about the peer or we purposefully deleted it.
}

// handleUpdatePeerHeight updates the heights of all peers who were known to announce a block we recently accepted.
func (s *ChainService) handleUpdatePeerHeights(state *peerState, umsg updatePeerHeightsMsg) {
	state.forAllPeers(
		func(sp *ServerPeer) {
			// The origin peer should already have the updated height.
			if sp == umsg.originPeer {
				return
			}
			// This is a pointer to the underlying memory which doesn't change.
			latestBlkHash := sp.LastAnnouncedBlock()
			// Skip this peer if it hasn't recently announced any new blocks.
			if latestBlkHash == nil {
				return
			}
			// If the peer has recently announced a block, and this block matches our newly accepted block, then update
			// their block height.
			if *latestBlkHash == *umsg.newHash {
				sp.UpdateLastBlockHeight(umsg.newHeight)
				sp.UpdateLastAnnouncedBlock(nil)
			}
		},
	)
}

// outboundPeerConnected is invoked by the connection manager when a new outbound connection is established. It
// initializes a new outbound server peer instance, associates it with the relevant state such as the connection request
// instance and the connection itself, and finally notifies the address manager of the attempt.
func (s *ChainService) outboundPeerConnected(c *connmgr.ConnReq, conn net.Conn) {
	sp := newServerPeer(s, c.Permanent)
	p, e := peer.NewOutboundPeer(newPeerConfig(sp), c.Addr.String())
	if e != nil {
		D.F("cannot create outbound peer %s: %s %s", c.Addr, e)
		s.connManager.Disconnect(c.ID())
	}
	sp.Peer = p
	sp.connReq = c
	sp.AssociateConnection(conn)
	go s.peerDoneHandler(sp)
	s.addrManager.Attempt(sp.NA())
}

// peerDoneHandler handles peer disconnects by notifiying the server that it's done along with other performing other
// desirable cleanup.
func (s *ChainService) peerDoneHandler(sp *ServerPeer) {
	sp.WaitForDisconnect()
	s.donePeers <- sp
	// Only tell block manager we are gone if we ever told it we existed.
	if sp.VersionKnown() {
		s.blockManager.DonePeer(sp)
	}
	sp.quit.Q()
}

// peerHandler is used to handle peer operations such as adding and removing peers to and from the server, banning
// peers, and broadcasting messages to peers. It must be run in a goroutine.
func (s *ChainService) peerHandler() {
	// Start the address manager and block manager, both of which are needed by peers. This is done here since their
	// lifecycle is closely tied to this handler and rather than adding more channels to synchronize things, it's easier
	// and slightly faster to simply start and stop them in this handler.
	s.addrManager.Start()
	s.blockManager.Start()
	e := s.utxoScanner.Start()
	if e != nil {
		D.Ln(e)
	}
	state := &peerState{
		persistentPeers: make(map[int32]*ServerPeer),
		outboundPeers:   make(map[int32]*ServerPeer),
		banned:          make(map[string]time.Time),
		outboundGroups:  make(map[string]int),
	}
	if !DisableDNSSeed {
		// Add peers discovered through DNS to the address manager.
		connmgr.SeedFromDNS(
			&s.chainParams, RequiredServices,
			s.nameResolver, func(addrs []*wire.NetAddress) {
				// Bitcoind uses a lookup of the dns seeder here. This is rather strange since the values looked up by
				// the DNS seed lookups will vary quite a lot. to replicate this behaviour we put all addresses as
				// having come from the first one.
				s.addrManager.AddAddresses(addrs, addrs[0])
			},
		)
	}
	go s.connManager.Start()
out:
	for {
		select {
		// New peers connected to the server.
		case p := <-s.newPeers:
			s.handleAddPeerMsg(state, p)
		// Disconnected peers.
		case p := <-s.donePeers:
			s.handleDonePeerMsg(state, p)
		// Block accepted in mainchain or orphan, update peer height.
		case umsg := <-s.peerHeightsUpdate:
			s.handleUpdatePeerHeights(state, umsg)
		// Peer to ban.
		case p := <-s.banPeers:
			s.handleBanPeerMsg(state, p)
		case qmsg := <-s.query:
			s.handleQuery(state, qmsg)
		case <-s.quit.Wait():
			// Disconnect all peers on server shutdown.
			state.forAllPeers(
				func(sp *ServerPeer) {
					F.Ln("shutdown peer", sp)
					sp.Disconnect()
				},
			)
			break out
		}
	}
	s.connManager.Stop()
	e = s.utxoScanner.Stop()
	if e != nil {
		D.Ln(e)
	}
	e = s.blockManager.Stop()
	if e != nil {
		D.Ln(e)
	}
	e = s.addrManager.Stop()
	if e != nil {
		D.Ln(e)
	}
	// Drain channels before exiting so nothing is left waiting around to send.
cleanup:
	for {
		select {
		case <-s.newPeers:
		case <-s.donePeers:
		case <-s.banPeers:
		case <-s.peerHeightsUpdate:
		case <-s.query:
		default:
			break cleanup
		}
	}
	s.wg.Done()
}

// rollBackToHeight rolls back all blocks until it hits the specified height. It sends notifications along the way.
func (s *ChainService) rollBackToHeight(height uint32) (*waddrmgr.BlockStamp, error) {
	header, headerHeight, e := s.BlockHeaders.ChainTip()
	if e != nil {
		return nil, e
	}
	bs := &waddrmgr.BlockStamp{
		Height: int32(headerHeight),
		Hash:   header.BlockHash(),
	}
	_, regHeight, e := s.RegFilterHeaders.ChainTip()
	if e != nil {
		return nil, e
	}
	for uint32(bs.Height) > height {
		header, _, e = s.BlockHeaders.FetchHeader(&bs.Hash)
		if e != nil {
			return nil, e
		}
		newTip := &header.PrevBlock
		// Only roll back filter headers if they've caught up this far.
		if uint32(bs.Height) <= regHeight {
			newFilterTip, e := s.RegFilterHeaders.RollbackLastBlock(newTip)
			if e != nil {
				return nil, e
			}
			regHeight = uint32(newFilterTip.Height)
		}
		bs, e = s.BlockHeaders.RollbackLastBlock()
		if e != nil {
			return nil, e
		}
		// Notifications are asynchronous, so we include the previous header in the disconnected notification in case
		// we're rolling back farther and the notification subscriber needs it but can't read it before it's deleted
		// from the store.
		//
		// TODO(aakselrod): Get rid of this when subscriptions are factored out into their own package.
		var lastHeader *wire.BlockHeader
		lastHeader, _, e = s.BlockHeaders.FetchHeader(newTip)
		if e != nil {
			return nil, e
		}
		s.mtxReorgHeader.Lock()
		s.reorgedBlockHeaders[header.PrevBlock] = lastHeader
		s.mtxReorgHeader.Unlock()
		// Now we send the block disconnected notifications.
		s.sendSubscribedMsg(
			&blockMessage{
				msgType: disconnect,
				header:  header,
			},
		)
	}
	return bs, nil
}

// OnAddr is invoked when a peer receives an addr bitcoin message and is used to notify the server about advertised
// addresses.
func (sp *ServerPeer) OnAddr(_ *peer.Peer, msg *wire.MsgAddr) {
	// Ignore addresses when running on the simulation test network. This helps prevent the network from becoming
	// another public test network since it will not be able to learn about other peers that have not specifically been
	// provided.
	if sp.server.chainParams.Net == chaincfg.SimNetParams.Net {
		return
	}
	// Ignore old style addresses which don't include a timestamp.
	if sp.ProtocolVersion() < wire.NetAddressTimeVersion {
		return
	}
	// A message that has no addresses is invalid.
	if len(msg.AddrList) == 0 {
		E.F(
			"command [%s] from %s does not contain any addresses",
			msg.Command(), sp.Addr(),
		)
		sp.Disconnect()
		return
	}
	for _, na := range msg.AddrList {
		// Don't add more address if we're disconnecting.
		if !sp.Connected() {
			return
		}
		// Set the timestamp to 5 days ago if it's more than 24 hours in the future so this address is one of the first
		// to be removed when space is needed.
		now := time.Now()
		if na.Timestamp.After(now.Add(time.Minute * 10)) {
			na.Timestamp = now.Add(-1 * time.Hour * 24 * 5)
		}
		// Add address to known addresses for this peer.
		sp.addKnownAddresses([]*wire.NetAddress{na})
	}
	// Add addresses to server address manager. The address manager handles the details of things such as preventing
	// duplicate addresses, max addresses, and last seen updates. XXX bitcoind gives a 2 hour time penalty here, do we
	// want to do the same?
	sp.server.addrManager.AddAddresses(msg.AddrList, sp.NA())
}

// OnFeeFilter is invoked when a peer receives a feefilter bitcoin message and is used by remote peers to request that
// no transactions which have a fee rate lower than provided value are inventoried to them. The peer will be
// disconnected if an invalid fee filter value is provided.
func (sp *ServerPeer) OnFeeFilter(_ *peer.Peer, msg *wire.MsgFeeFilter) {
	// Chk that the passed minimum fee is a valid amount.
	if msg.MinFee < 0 || msg.MinFee > int64(amt.MaxSatoshi) {
		D.F(
			"peer %v sent an invalid feefilter '%v' -- disconnecting %s",
			sp, amt.Amount(msg.MinFee),
		)
		sp.Disconnect()
		return
	}
	atomic.StoreInt64(&sp.feeFilter, msg.MinFee)
}

// OnHeaders is invoked when a peer receives a headers bitcoin message. The message is passed down to the block manager.
func (sp *ServerPeer) OnHeaders(p *peer.Peer, msg *wire.MsgHeaders) {
	T.F(
		"got headers with %d items from %s",
		len(msg.Headers), p.Addr(),
	)
	sp.server.blockManager.QueueHeaders(msg, sp)
}

// OnInv is invoked when a peer receives an inv bitcoin message and is used to examine the inventory being advertised by
// the remote peer and react accordingly. We pass the message down to blockmanager which will call QueueMessage with any
// appropriate responses.
func (sp *ServerPeer) OnInv(p *peer.Peer, msg *wire.MsgInv) {
	T.F(
		"got inv with %d items from %s", len(msg.InvList), p.Addr(),
	)
	newInv := wire.NewMsgInvSizeHint(uint(len(msg.InvList)))
	for _, invVect := range msg.InvList {
		if invVect.Type == wire.InvTypeTx {
			T.F(
				"ignoring tx %s in inv from %v -- SPV mode",
				invVect.Hash, sp,
			)
			if sp.ProtocolVersion() >= wire.BIP0037Version {
				I.F(
					"peer %v is announcing transactions -- disconnecting", sp,
				)
				sp.Disconnect()
				return
			}
			continue
		}
		e := newInv.AddInvVect(invVect)
		if e != nil {
			E.Ln("failed to add inventory vector:", e)
			break
		}
	}
	if len(newInv.InvList) > 0 {
		sp.server.blockManager.QueueInv(newInv, sp)
	}
}

// OnRead is invoked when a peer receives a message and it is used to update the bytes received by the server.
func (sp *ServerPeer) OnRead(
	_ *peer.Peer, bytesRead int, msg wire.Message,
	e error,
) {
	sp.server.AddBytesReceived(uint64(bytesRead))
	// Send a message to each subscriber. Each message gets its own goroutine to prevent blocking on the mutex lock.
	// TODO: Flood control.
	sp.mtxSubscribers.RLock()
	defer sp.mtxSubscribers.RUnlock()
	for subscription := range sp.recvSubscribers {
		go func(subscription spMsgSubscription) {
			select {
			case <-subscription.quitChan.Wait():
			case subscription.msgChan <- spMsg{
				msg: msg,
				sp:  sp,
			}:
			}
		}(subscription)
	}
}

// OnReject is invoked when a peer receives a reject bitcoin message and is used to notify the server about a rejected
// transaction.
func (sp *ServerPeer) OnReject(_ *peer.Peer, msg *wire.MsgReject) {
	// TODO(roaseef): log?
}

// OnVerAck is invoked when a peer receives a verack bitcoin message and is used to send the "sendheaders" command to
// peers that are of a sufficienty new protocol version.
func (sp *ServerPeer) OnVerAck(_ *peer.Peer, msg *wire.MsgVerAck) {
	e := sp.pushSendHeadersMsg()
	if e != nil {
		D.Ln(e)
	}
}

// OnVersion is invoked when a peer receives a version bitcoin message and is used to negotiate the protocol version
// details as well as kick start the communications.
func (sp *ServerPeer) OnVersion(_ *peer.Peer, msg *wire.MsgVersion) *wire.MsgReject {
	// Add the remote peer time as a sample for creating an offset against the local clock to keep the network time in
	// sync.
	sp.server.timeSource.AddTimeSample(sp.Addr(), msg.Timestamp)
	// Chk to see if the peer supports the latest protocol version and service bits required to service us. If not,
	// then we'll disconnect so we can find compatible peers.
	peerServices := sp.Services()
	// if peerServices&wire.SFNodeWitness != wire.SFNodeWitness ||
	if peerServices&wire.SFNodeCF != wire.SFNodeCF {
		I.F(
			"disconnecting peer %v, cannot serve compact filters", sp,
		)
		sp.Disconnect()
		return nil
	}
	// Signal the block manager this peer is a new sync candidate.
	sp.server.blockManager.NewPeer(sp)
	// Update the address manager and request known addresses from the remote peer for outbound connections. This is
	// skipped when running on the simulation test network since it is only intended to connect to specified peers and
	// actively avoids advertising and connecting to discovered peers.
	if sp.server.chainParams.Net != chaincfg.SimNetParams.Net {
		addrManager := sp.server.addrManager
		// Request known addresses if the server address manager needs more and the peer has a protocol version new
		// enough to include a timestamp with addresses.
		hasTimestamp := sp.ProtocolVersion() >=
			wire.NetAddressTimeVersion
		if addrManager.NeedMoreAddresses() && hasTimestamp {
			sp.QueueMessage(wire.NewMsgGetAddr(), nil)
		}
		// Mark the address as a known good address.
		addrManager.Good(sp.NA())
	}
	// Add valid peer to the server.
	sp.server.AddPeer(sp)
	return nil
}

// OnWrite is invoked when a peer sends a message and it is used to update the bytes sent by the server.
func (sp *ServerPeer) OnWrite(_ *peer.Peer, bytesWritten int, msg wire.Message, e error) {
	sp.server.AddBytesSent(uint64(bytesWritten))
}

// // addBanScore increases the persistent and decaying ban score fields by the
// // values passed as parameters. If the resulting score exceeds half of the ban
// // threshold, a warning is logged including the reason provided. Further, if
// // the score is above the ban threshold, the peer will be banned and
// // disconnected.
// func (sp *ServerPeer) addBanScore(persistent, transient uint32, reason string) {
// 	// No warning is logged and no score is calculated if banning is disabled.
// 	warnThreshold := BanThreshold >> 1
// 	if transient == 0 && persistent == 0 {
// 		// The score is not being increased, but a warning message is still
// 		// logged if the score is above the warn threshold.
// 		score := sp.banScore.Int()
// 		if score > warnThreshold {
// 			WARNF{
// 				"misbehaving peer %s: %s -- ban score is %d, it was not increased this time",
// 				sp, reason, score,
// 			}
// 		}
// 		return
// 	}
// 	score := sp.banScore.Increase(persistent, transient)
// 	if score > warnThreshold {
// 		WARNF{
// 			"misbehaving peer %s: %s -- ban score increased to %d",
// 			sp, reason, score,
// 		}
// 		if score > BanThreshold {
// 			WARNF{
// 				"misbehaving peer %s -- banning and disconnecting",
// 				sp,
// 			}
// 			sp.server.BanPeer(sp)
// 			sp.Disconnect()
// 		}
// 	}
// }

// addKnownAddresses adds the given addresses to the set of known addresses to the peer to prevent sending duplicate
// addresses.
func (sp *ServerPeer) addKnownAddresses(addresses []*wire.NetAddress) {
	for _, na := range addresses {
		sp.knownAddresses[addrmgr.NetAddressKey(na)] = struct{}{}
	}
}

// // addressKnown true if the given address is already known to the peer.
// func (sp *ServerPeer) addressKnown(na *wire.NetAddress) bool {
// 	_, exists := sp.knownAddresses[addrmgr.NetAddressKey(na)]
// 	return exists
// }

// newestBlock returns the current best block hash and height using the format required by the configuration for the
// peer package.
func (sp *ServerPeer) newestBlock() (*chainhash.Hash, int32, error) {
	bestHeader, bestHeight, e := sp.server.BlockHeaders.ChainTip()
	if e != nil {
		return nil, 0, e
	}
	bestHash := bestHeader.BlockHash()
	return &bestHash, int32(bestHeight), nil
}

// pushSendHeadersMsg sends a sendheaders message to the connected peer.
func (sp *ServerPeer) pushSendHeadersMsg() (e error) {
	if sp.VersionKnown() {
		if sp.ProtocolVersion() > wire.SendHeadersVersion {
			sp.QueueMessage(wire.NewMsgSendHeaders(), nil)
		}
	}
	return nil
}

// subscribeRecvMsg handles adding OnRead subscriptions to the server peer.
func (sp *ServerPeer) subscribeRecvMsg(subscription spMsgSubscription) {
	sp.mtxSubscribers.Lock()
	defer sp.mtxSubscribers.Unlock()
	sp.recvSubscribers[subscription] = struct{}{}
}

// unsubscribeRecvMsgs handles removing OnRead subscriptions from the server peer.
func (sp *ServerPeer) unsubscribeRecvMsgs(subscription spMsgSubscription) {
	sp.mtxSubscribers.Lock()
	defer sp.mtxSubscribers.Unlock()
	delete(sp.recvSubscribers, subscription)
}

// Count returns the count of all known peers.
func (ps *peerState) Count() int {
	return len(ps.outboundPeers) + len(ps.persistentPeers)
}

// forAllOutboundPeers is a helper function that runs closure on all outbound peers known to peerState.
func (ps *peerState) forAllOutboundPeers(closure func(sp *ServerPeer)) {
	for _, e := range ps.outboundPeers {
		closure(e)
	}
	for _, e := range ps.persistentPeers {
		closure(e)
	}
}

// forAllPeers is a helper function that runs closure on all peers known to peerState.
func (ps *peerState) forAllPeers(closure func(sp *ServerPeer)) {
	ps.forAllOutboundPeers(closure)
}

// NewChainService returns a new chain service configured to connect to the bitcoin network type specified by
// chainParams. Use start to begin syncing with peers.
func NewChainService(cfg Config) (*ChainService, error) {
	// First, we'll txsort out the methods that we'll use to established outbound TCP connections, as well as perform any
	// DNS queries.
	//
	// If the dialler was specified, then we'll use that in place of the default net.Dial function.
	var (
		nameResolver func(string) ([]net.IP, error)
		dialer       func(net.Addr) (net.Conn, error)
	)
	if cfg.Dialer != nil {
		dialer = cfg.Dialer
	} else {
		dialer = func(addr net.Addr) (net.Conn, error) {
			return net.Dial(addr.Network(), addr.String())
		}
	}
	// Similarly, if the user specified as function to use for name resolution, then we'll use that everywhere as well.
	if cfg.NameResolver != nil {
		nameResolver = cfg.NameResolver
	} else {
		nameResolver = net.LookupIP
	}
	// When creating the addr manager, we'll check to see if the user has provided their own resolution function. If so,
	// then we'll use that instead as this may be proxying requests over an anonymizing network.
	amgr := addrmgr.New(cfg.DataDir, nameResolver)
	s := ChainService{
		chainParams:         cfg.ChainParams,
		addrManager:         amgr,
		newPeers:            make(chan *ServerPeer, MaxPeers),
		donePeers:           make(chan *ServerPeer, MaxPeers),
		banPeers:            make(chan *ServerPeer, MaxPeers),
		query:               make(chan interface{}),
		quit:                qu.T(),
		peerHeightsUpdate:   make(chan updatePeerHeightsMsg),
		timeSource:          blockchain.NewMedianTime(),
		services:            Services,
		userAgentName:       UserAgentName,
		userAgentVersion:    UserAgentVersion,
		blockSubscribers:    make(map[*blockSubscription]struct{}),
		reorgedBlockHeaders: make(map[chainhash.Hash]*wire.BlockHeader),
		nameResolver:        nameResolver,
		dialer:              dialer,
	}
	// We set the queryPeers method to point to queryChainServicePeers, passing a reference to the newly created
	// ChainService.
	s.queryPeers = func(
		msg wire.Message, f func(
		*ServerPeer,
		wire.Message, chan<- struct{},
	), qo ...QueryOption,
	) {
		queryChainServicePeers(&s, msg, f, qo...)
	}
	// We do the same for queryBatch.
	s.queryBatch = func(
		msgs []wire.Message, f func(
		*ServerPeer,
		wire.Message, wire.Message,
	) bool, q qu.C,
		qo ...QueryOption,
	) {
		queryChainServiceBatch(&s, msgs, f, q, qo...)
	}
	var e error
	s.FilterDB, e = filterdb.New(cfg.Database, cfg.ChainParams)
	if e != nil {
		return nil, e
	}
	filterCacheSize := DefaultFilterCacheSize
	if cfg.FilterCacheSize != 0 {
		filterCacheSize = cfg.FilterCacheSize
	}
	s.FilterCache = lru.NewCache(filterCacheSize)
	blockCacheSize := DefaultBlockCacheSize
	if cfg.BlockCacheSize != 0 {
		blockCacheSize = cfg.BlockCacheSize
	}
	s.BlockCache = lru.NewCache(blockCacheSize)
	s.BlockHeaders, e = headerfs.NewBlockHeaderStore(
		cfg.DataDir, cfg.Database, &cfg.ChainParams,
	)
	if e != nil {
		return nil, e
	}
	s.RegFilterHeaders, e = headerfs.NewFilterHeaderStore(
		cfg.DataDir, cfg.Database, headerfs.RegularFilter, &cfg.ChainParams,
	)
	if e != nil {
		return nil, e
	}
	bm, e := newBlockManager(&s)
	if e != nil {
		return nil, e
	}
	s.blockManager = bm
	// Only setup a function to return new addresses to connect to when not running in connect-only mode. The simulation
	// network is always in connect-only mode since it is only intended to connect to specified peers and actively avoid
	// advertising and connecting to discovered peers in order to prevent it from becoming a public test network.
	var newAddressFunc func() (net.Addr, error)
	if s.chainParams.Net != chaincfg.SimNetParams.Net {
		newAddressFunc = func() (net.Addr, error) {
			for tries := 0; tries < 100; tries++ {
				addr := s.addrManager.GetAddress()
				if addr == nil {
					break
				}
				// Address will not be invalid, local or unroutable because addrmanager rejects those on addition. Just
				// check that we don't already have an address in the same group so that we are not connecting to the
				// same network segment at the expense of others.
				key := addrmgr.GroupKey(addr.NetAddress())
				if s.OutboundGroupCount(key) != 0 {
					continue
				}
				// only allow recent nodes (10mins) after we failed 30 times
				if tries < 30 && time.Since(addr.LastAttempt()) < 10*time.Minute {
					continue
				}
				// allow nondefault ports after 50 failed tries.
				if tries < 50 && fmt.Sprintf("%d", addr.NetAddress().Port) !=
					s.chainParams.DefaultPort {
					continue
				}
				addrString := addrmgr.NetAddressKey(addr.NetAddress())
				return s.addrStringToNetAddr(addrString)
			}
			return nil, errors.New("no valid connect address")
		}
	}
	cmgrCfg := &connmgr.Config{
		RetryDuration:  ConnectionRetryInterval,
		TargetOutbound: uint32(TargetOutbound),
		OnConnection:   s.outboundPeerConnected,
		Dial:           dialer,
	}
	if len(cfg.ConnectPeers) == 0 {
		cmgrCfg.GetNewAddress = newAddressFunc
	}
	// Create a connection manager.
	if MaxPeers < TargetOutbound {
		TargetOutbound = MaxPeers
	}
	cmgr, e := connmgr.New(cmgrCfg)
	if e != nil {
		return nil, e
	}
	s.connManager = cmgr
	// Start up persistent peers.
	permanentPeers := cfg.ConnectPeers
	if len(permanentPeers) == 0 {
		permanentPeers = cfg.AddPeers
	}
	for _, addr := range permanentPeers {
		tcpAddr, e := s.addrStringToNetAddr(addr)
		if e != nil {
			return nil, e
		}
		go s.connManager.Connect(
			&connmgr.ConnReq{
				Addr:      tcpAddr,
				Permanent: true,
			},
		)
	}
	s.utxoScanner = NewUtxoScanner(
		&UtxoScannerConfig{
			BestSnapshot:       s.BestBlock,
			GetBlockHash:       s.GetBlockHash,
			BlockFilterMatches: s.blockFilterMatches,
			GetBlock:           s.GetBlock,
		},
	)
	return &s, nil
}

// disconnectPeer attempts to drop the connection of a tageted peer in the passed peer list. Targets are identified via
// usage of the passed `compareFunc`, which should return `true` if the passed peer is the target peer. This function
// returns true on success and false if the peer is unable to be located. If the peer is found, and the passed callback:
// `whenFound' isn't nil, we call it with the peer as the argument before it is removed from the peerList, and is
// disconnected from the server.
func disconnectPeer(
	peerList map[int32]*ServerPeer,
	compareFunc func(*ServerPeer) bool,
	whenFound func(*ServerPeer),
) bool {
	for addr, serverPeer := range peerList {
		if compareFunc(serverPeer) {
			if whenFound != nil {
				whenFound(serverPeer)
			}
			// This is ok because we are not continuing to iterate so won't corrupt the loop.
			delete(peerList, addr)
			serverPeer.Disconnect()
			return true
		}
	}
	return false
}

// newPeerConfig returns the configuration for the given ServerPeer.
func newPeerConfig(sp *ServerPeer) *peer.Config {
	return &peer.Config{
		Listeners: peer.MessageListeners{
			OnVersion: sp.OnVersion,
			// OnVerAck:    sp.OnVerAck, // Don't use sendheaders yet
			OnInv:       sp.OnInv,
			OnHeaders:   sp.OnHeaders,
			OnReject:    sp.OnReject,
			OnFeeFilter: sp.OnFeeFilter,
			OnAddr:      sp.OnAddr,
			OnRead:      sp.OnRead,
			OnWrite:     sp.OnWrite,
			// Note: The reference client currently bans peers that send alerts not signed with its key. We could verify
			// against their key, but since the reference client is currently unwilling to support other
			// implementations' alert messages, we will not relay theirs.
			OnAlert: nil,
		},
		NewestBlock:      sp.newestBlock,
		HostToNetAddress: sp.server.addrManager.HostToNetAddress,
		UserAgentName:    sp.server.userAgentName,
		UserAgentVersion: sp.server.userAgentVersion,
		ChainParams:      &sp.server.chainParams,
		Services:         sp.server.services,
		ProtocolVersion:  wire.FeeFilterVersion,
		DisableRelayTx:   true,
	}
}

// newServerPeer returns a new ServerPeer instance. The peer needs to be set by the caller.
func newServerPeer(s *ChainService, isPersistent bool) *ServerPeer {
	return &ServerPeer{
		server:          s,
		persistent:      isPersistent,
		knownAddresses:  make(map[string]struct{}),
		quit:            qu.T(),
		recvSubscribers: make(map[spMsgSubscription]struct{}),
	}
}
