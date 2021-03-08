package peer_test

import (
	"fmt"
	"net"
	"time"
	
	qu "github.com/p9c/pod/pkg/util/quit"
	
	"github.com/p9c/pod/pkg/chain/config/netparams"
	"github.com/p9c/pod/pkg/chain/wire"
	"github.com/p9c/pod/pkg/comm/peer"
)

// mockRemotePeer creates a basic inbound peer listening on the simnet port for use with Example_peerConnection. It does
// not return until the listner is active.
func mockRemotePeer() (e error) {
	// Configure peer to act as a simnet node that offers no services.
	peerCfg := &peer.Config{
		UserAgentName:    "peer",  // User agent name to advertise.
		UserAgentVersion: "1.0.0", // User agent version to advertise.
		ChainParams:      &netparams.SimNetParams,
		TrickleInterval:  time.Second * 10,
	}
	// Accept connections on the simnet port.
	listener, e := net.Listen("tcp", "127.0.0.1:18555")
	if e != nil  {
		return err
	}
	go func() {
		conn, e := listener.Accept()
		if e != nil  {
			peer.Errorf("Accept: error %v", err)
			return
		}
		// Create and start the inbound peer.
		p := peer.NewInboundPeer(peerCfg)
		p.AssociateConnection(conn)
	}()
	return nil
}

// This example demonstrates the basic process for initializing and creating an outbound peer. Peers negotiate by
// exchanging version and verack messages. For demonstration, a simple handler for version message is attached to the
// peer.
func Example_newOutboundPeer() {
	// Ordinarily this will not be needed since the outbound peer will be connecting to a remote peer, however, since this example is executed and tested, a mock remote peer is needed to listen for the outbound peer.
	if e := mockRemotePeer(); dbg.Chk(e) {
		peer.Errorf("mockRemotePeer: unexpected error %v", err)
		return
	}
	// Create an outbound peer that is configured to act as a simnet node that offers no services and has listeners for the version and verack messages.  The verack listener is used here to signal the code below when the handshake has been finished by signalling a channel.
	verack := qu.T()
	peerCfg := &peer.Config{
		UserAgentName:    "peer",  // User agent name to advertise.
		UserAgentVersion: "1.0.0", // User agent version to advertise.
		ChainParams:      &netparams.SimNetParams,
		Services:         0,
		TrickleInterval:  time.Second * 10,
		Listeners: peer.MessageListeners{
			OnVersion: func(p *peer.Peer, msg *wire.MsgVersion) *wire.MsgReject {
				fmt.Println("outbound: received version")
				return nil
			},
			OnVerAck: func(p *peer.Peer, msg *wire.MsgVerAck) {
				verack <- struct{}{}
			},
		},
	}
	p, e := peer.NewOutboundPeer(peerCfg, "127.0.0.1:18555")
	if e != nil  {
		peer.Errorf("NewOutboundPeer: error %v", err)
		return
	}
	// Establish the connection to the peer address and mark it connected.
	conn, e := net.Dial("tcp", p.Addr())
	if e != nil  {
		peer.Errorf("net.Dial: error %v", err)
		return
	}
	p.AssociateConnection(conn)
	// Wait for the verack message or timeout in case of failure.
	select {
	case <-verack.Wait():
	case <-time.After(time.Second * 1):
		peer.Error("Example_peerConnection: verack timeout")
	}
	// Disconnect the peer.
	p.Disconnect()
	p.WaitForDisconnect()
	// Output:
	// outbound: received version
}
