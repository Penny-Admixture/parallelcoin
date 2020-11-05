package gui

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net"
	"runtime/pprof"
	"strings"
	"time"

	l "gioui.org/layout"

	chainhash "github.com/p9c/pod/pkg/chain/hash"
	"github.com/p9c/pod/pkg/rpc/btcjson"
	rpcclient "github.com/p9c/pod/pkg/rpc/client"
	"github.com/p9c/pod/pkg/util"
)

func (wg *WalletGUI) updateThingies() (err error) {
	// update the configuration
	var b []byte
	if b, err = ioutil.ReadFile(*wg.cx.Config.ConfigFile); !Check(err) {
		if err = json.Unmarshal(b, wg.cx.Config); !Check(err) {
			return
		}
	}
	return
}

func (wg *WalletGUI) chainClient() (err error) {
	if err = wg.updateThingies(); Check(err) {
	}
	wg.ChainClient, err = rpcclient.New(&rpcclient.ConnConfig{
		Host:         *wg.cx.Config.RPCConnect,
		User:         *wg.cx.Config.Username,
		Pass:         *wg.cx.Config.Password,
		HTTPPostMode: true,
	}, nil)
	return
}

func (wg *WalletGUI) walletClient() (err error) {
	if err = wg.updateThingies(); Check(err) {
	}
	// update wallet data
	walletRPC := (*wg.cx.Config.WalletRPCListeners)[0]
	var walletServer, port string
	if _, port, err = net.SplitHostPort(walletRPC); !Check(err) {
		walletServer = net.JoinHostPort("127.0.0.1", port)
	}
	wg.WalletClient, err = rpcclient.New(&rpcclient.ConnConfig{
		Host:         walletServer,
		User:         *wg.cx.Config.Username,
		Pass:         *wg.cx.Config.Password,
		HTTPPostMode: true,
	}, nil)
	return
}

func (wg *WalletGUI) Tickers() {
	go func() {
		var err error
		seconds := time.Tick(time.Second)
		// fiveSeconds := time.Tick(time.Second * 5)
	totalOut:
		for {
		preconnect:
			for {
				select {
				case <-seconds:
					if wg.ChainClient != nil {
						wg.ChainClient.Disconnect()
						if wg.ChainClient.Disconnected() {
							wg.ChainClient = nil
						}
					}
					if err = wg.chainClient(); Check(err) {
						break
					}
					if wg.WalletClient != nil {
						wg.WalletClient.Disconnect()
						if wg.WalletClient.Disconnected() {
							wg.WalletClient = nil
						}
					}
					if err = wg.walletClient(); Check(err) {
						break
					}
					// if we got to here both are connected
					break preconnect
				case <-wg.quit:
					break totalOut
				}
			}
		out:
			for {
				select {
				case <-seconds:
					// Debug("connectChainRPC ticker")
					var err error

					var height int32
					var h *chainhash.Hash
					if h, height, err = wg.ChainClient.GetBestBlock(); Check(err) {
						break out
					}
					wg.State.SetBestBlockHeight(int(height))
					wg.State.SetBestBlockHash(h)
					// // update wallet data
					// walletRPC := (*wg.cx.Config.WalletRPCListeners)[0]
					// var walletServer, port string
					// if _, port, err = net.SplitHostPort(walletRPC); !Check(err) {
					//	walletServer = net.JoinHostPort("127.0.0.1", port)
					// }
					// walletConnConfig := &rpcclient.ConnConfig{
					//	Host:         walletServer,
					//	User:         *wg.cx.Config.Username,
					//	Pass:         *wg.cx.Config.Password,
					//	HTTPPostMode: true,
					// }
					var unconfirmed util.Amount
					if unconfirmed, err = wg.WalletClient.GetUnconfirmedBalance("default"); Check(err) {
						break out
					}
					wg.State.SetBalanceUnconfirmed(unconfirmed.ToDUO())
					var confirmed util.Amount
					if confirmed, err = wg.WalletClient.GetBalance("default"); Check(err) {
						break out
					}
					wg.State.SetBalance(confirmed.ToDUO())
					var ltr []btcjson.ListTransactionsResult
					// TODO: for some reason this function returns half as many as requested
					if ltr, err = wg.WalletClient.ListTransactionsCount("default", 20); Check(err) {
						break out
					}
					// Debugs(ltr)
					wg.State.SetLastTxs(ltr)
					// case <-fiveSeconds:
					var b []byte
					buf := bytes.NewBuffer(b)
					if err = pprof.Lookup("goroutine").WriteTo(buf, 2); Check(err) {
						break out
					}
					// Debug(buf.String())
					lines := strings.Split(buf.String(), "\n")
					var out []l.Widget
					// var outString string
					for i := range lines {
						var text string
						if strings.HasPrefix(lines[i], "goroutine") && i < len(lines)-2 {
							text = lines[i+2]
							text = strings.TrimSpace(strings.Split(text, " ")[0])
							// outString += text + "\n"
							out = append(out, wg.th.Caption(text).Color("DocText").Fn)
						}
					}
					// Debug(outString)
					wg.State.SetGoroutines(out)
				case <-wg.quit:
					break totalOut
				}
			}
		}

	}()
}
