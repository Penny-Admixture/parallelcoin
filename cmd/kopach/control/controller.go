package control

import (
	"container/ring"
	"fmt"
	"github.com/VividCortex/ewma"
	"github.com/p9c/pod/app/save"
	"github.com/p9c/pod/cmd/kopach/control/job"
	"github.com/p9c/pod/cmd/kopach/control/templates"
	"github.com/p9c/pod/cmd/walletmain"
	"github.com/p9c/pod/pkg/util/routeable"
	"github.com/urfave/cli"
	"net"
	"time"
	
	rpcclient "github.com/p9c/pod/pkg/rpc/client"
	"github.com/p9c/pod/pkg/util/quit"
	
	"go.uber.org/atomic"
	
	"github.com/p9c/pod/app/conte"
	"github.com/p9c/pod/cmd/kopach/control/p2padvt"
	"github.com/p9c/pod/cmd/kopach/control/pause"
	"github.com/p9c/pod/pkg/chain/mining"
	"github.com/p9c/pod/pkg/comm/transport"
	rav "github.com/p9c/pod/pkg/data/ring"
	"github.com/p9c/pod/pkg/util/interrupt"
)


type Controller struct {
	multiConn              *transport.Channel
	active                 atomic.Bool
	quit                   qu.C
	cx                     *conte.Xt
	isMining               atomic.Bool
	height                 atomic.Int32
	blockTemplateGenerator *mining.BlkTmplGenerator
	msgBlockTemplate       *templates.Message
	oldBlocks              atomic.Value
	lastTxUpdate           atomic.Value
	lastGenerated          atomic.Value
	pauseShards            [][]byte
	sendAddresses          []*net.UDPAddr
	buffer                 *ring.Ring
	began                  time.Time
	otherNodes             map[uint64]*nodeSpec
	uuid                   uint64
	hashCount              atomic.Uint64
	hashSampleBuf          *rav.BufferUint64
	lastNonce              int32
	walletClient           *rpcclient.Client
}

// Run starts up a controller
func Run(cx *conte.Xt) (quit qu.C) {
	if *cx.Config.DisableController {
		Info("controller is disabled")
		return
	}
	cx.Controller.Store(true)
	if len(*cx.Config.RPCListeners) < 1 || *cx.Config.DisableRPC {
		Warn("not running controller without RPC enabled")
		return
	}
	if len(*cx.Config.P2PListeners) < 1 || *cx.Config.DisableListen {
		Warn("not running controller without p2p listener enabled", *cx.Config.P2PListeners)
		return
	}
	nS := make(map[uint64]*nodeSpec)
	c := &Controller{
		quit:                   qu.T(),
		cx:                     cx,
		sendAddresses:          []*net.UDPAddr{},
		blockTemplateGenerator: getBlkTemplateGenerator(cx),
		buffer:                 ring.New(BufferSize),
		began:                  time.Now(),
		otherNodes:             nS,
		uuid:                   cx.UUID,
		hashSampleBuf:          rav.NewBufferUint64(100),
	}
	c.isMining.Store(true)
	// maintain connection to wallet if it is available
	var err error
	go c.walletRPCWatcher()
	// c.prevHash.Store(&chainhash.Hash{})
	quit = c.quit
	c.lastTxUpdate.Store(time.Now().UnixNano())
	c.lastGenerated.Store(time.Now().UnixNano())
	c.height.Store(0)
	c.active.Store(false)
	if c.multiConn, err = transport.NewBroadcastChannel(
		"controller", c, *cx.Config.MinerPass, transport.DefaultPort, MaxDatagramSize, handlersMulticast,
		quit,
	); Check(err) {
		c.quit.Q()
		return
	}
	if c.pauseShards = transport.GetShards(p2padvt.Get(cx)); Check(err) {
	} else {
		c.active.Store(true)
	}
	interrupt.AddHandler(
		func() {
			Debug("miner controller shutting down")
			c.active.Store(false)
			if err = c.multiConn.SendMany(pause.Magic, c.pauseShards); Check(err) {
			}
			if err = c.multiConn.Close(); Check(err) {
			}
			c.quit.Q()
		},
	)
	Debug("sending broadcasts to:", UDP4MulticastAddress)
	
	go c.advertiserAndRebroadcaster()
	return
}

func (c *Controller) hashReport() float64 {
	c.hashSampleBuf.Add(c.hashCount.Load())
	av := ewma.NewMovingAverage()
	var i int
	var prev uint64
	if err := c.hashSampleBuf.ForEach(
		func(v uint64) error {
			if i < 1 {
				prev = v
			} else {
				interval := v - prev
				av.Add(float64(interval))
				prev = v
			}
			i++
			return nil
		},
	); Check(err) {
	}
	return av.Value()
}

func (c *Controller) walletRPCWatcher() {
	Debug("starting wallet rpc connection watcher for mining addresses")
	var err error
	backoffTime := time.Second
	certs := walletmain.ReadCAFile(c.cx.Config)
totalOut:
	for {
	trying:
		for {
			select {
			case <-c.cx.KillAll.Wait():
				break totalOut
			default:
			}
			Debug("trying to connect to wallet for mining addresses...")
			// If we can reach the wallet configured in the same datadir we can mine
			if c.walletClient, err = rpcclient.New(
				&rpcclient.ConnConfig{
					Host:         *c.cx.Config.WalletServer,
					Endpoint:     "ws",
					User:         *c.cx.Config.Username,
					Pass:         *c.cx.Config.Password,
					TLS:          *c.cx.Config.TLS,
					Certificates: certs,
				}, nil, c.cx.KillAll,
			); Check(err) {
				Debug("failed, will try again")
				c.isMining.Store(false)
				select {
				case <-time.After(backoffTime):
				case <-c.quit.Wait():
					c.isMining.Store(false)
					break totalOut
				}
				if backoffTime <= time.Second*5 {
					backoffTime += time.Second
				}
				continue
			} else {
				Debug("<<<controller has wallet connection>>>")
				c.isMining.Store(true)
				backoffTime = time.Second
				break trying
			}
		}
		Debug("<<<connected to wallet>>>")
		retryTicker := time.NewTicker(time.Second)
	connected:
		for {
			select {
			case <-retryTicker.C:
				if c.walletClient.Disconnected() {
					c.isMining.Store(false)
					break connected
				}
			case <-c.quit.Wait():
				c.isMining.Store(false)
				break totalOut
			}
		}
		Debug("disconnected from wallet")
	}
}

func (c *Controller) advertiserAndRebroadcaster() {
	if !c.active.Load() {
		Info("ready to send out jobs!")
		c.active.Store(true)
	}
	ticker := time.NewTicker(time.Second)
	const countTick = 10
	counter := countTick / 2
	once := false
	var err error
out:
	for {
		select {
		case <-ticker.C:
			c.height.Store(c.cx.RPCServer.Cfg.Chain.BestSnapshot().Height)
			if c.isMining.Load() {
				if !once {
					// c.cx.RealNode.Chain.Subscribe(c.chainNotifier())
					once = true
					c.active.Store(true)
				}
			}
			if counter%countTick == 0 {
				j := p2padvt.GetAdvt(c.cx)
				if *c.cx.Config.AutoListen {
					*c.cx.Config.P2PConnect = cli.StringSlice{}
					_, addresses := routeable.GetAllInterfacesAndAddresses()
					Traces(addresses)
					for i := range addresses {
						addrS := net.JoinHostPort(addresses[i].IP.String(), fmt.Sprint(j.P2P))
						*c.cx.Config.P2PConnect = append(*c.cx.Config.P2PConnect, addrS)
					}
					save.Pod(c.cx.Config)
				}
			}
			counter++
			// send out advertisment
			if err = c.multiConn.SendMany(p2padvt.Magic, transport.GetShards(p2padvt.Get(c.cx))); Check(err) {
			}
			if c.isMining.Load() {
				Debug("updating and sending out new work")
				if err = c.updateAndSendWork(); Check(err) {
				}
			}
		case <-c.quit.Wait():
			Debug("quitting on close quit channel")
			break out
		case <-c.cx.NodeKill.Wait():
			Debug("quitting on NodeKill")
			c.quit.Q()
			break out
		case <-c.cx.KillAll.Wait():
			Debug("quitting on KillAll")
			c.quit.Q()
			break out
		}
	}
	c.active.Store(false)
	Debug("controller exiting")
}

func (c *Controller) SendShards(magic []byte, data [][]byte) (err error) {
	if err = c.multiConn.SendMany(magic, data); Check(err) {
	}
	return
}

func (c *Controller) updateAndSendWork() (err error) {
	var getNew bool
	// The current block is stale if the best block has changed.
	oB, ok := c.oldBlocks.Load().([][]byte)
	switch {
	case !ok:
		Trace("cached template is nil")
		getNew = true
	case len(oB) == 0:
		Trace("cached template is zero length")
		getNew = true
	// case c.msgBlockTemplate.PrevBlock != prev.BlockHash():
	// 	Debug("new best block hash")
	// 	getNew = true
	case c.lastTxUpdate.Load() != c.blockTemplateGenerator.GetTxSource().LastUpdated() &&
		time.Now().After(time.Unix(0, c.lastGenerated.Load().(int64)+int64(time.Minute))):
		Trace("block is stale, regenerating")
		getNew = true
		c.lastTxUpdate.Store(time.Now().UnixNano())
		c.lastGenerated.Store(time.Now().UnixNano())
	}
	if getNew {
		// if oB, err = c.GetTemplateMessageShards(); Check(err) {
		// 	return
		// }
	}
	if err = c.SendShards(job.Magic, oB); Check(err) {
	}
	c.oldBlocks.Store(oB)
	return
}
