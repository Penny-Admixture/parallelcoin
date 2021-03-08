package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"path/filepath"
	"time"
	
	qu "github.com/p9c/pod/pkg/util/quit"
	
	"github.com/davecgh/go-spew/spew"
	
	"github.com/p9c/pod/app/appdata"
	rpcclient "github.com/p9c/pod/pkg/rpc/client"
	"github.com/p9c/pod/pkg/util"
)

func main() {
	// Only override the handlers for notifications you care about. Also note most of the handlers will only be called
	// if you register for notifications. See the documentation of the rpcclient NotificationHandlers type for more
	// details about each handler.
	ntfnHandlers := rpcclient.NotificationHandlers{
		OnAccountBalance: func(account string, balance util.Amount, confirmed bool) {
			log.Printf("New balance for account %s: %v", account,
				balance)
		},
	}
	// Connect to local btcwallet RPC server using websockets.
	certHomeDir := appdata.Dir("mod", false)
	certs, e := ioutil.ReadFile(filepath.Join(certHomeDir, "rpc.cert"))
	if e != nil  {
		ftl.Ln(err)
	}
	connCfg := &rpcclient.ConnConfig{
		Host:         "localhost:11046",
		Endpoint:     "ws",
		User:         "yourrpcuser",
		Pass:         "yourrpcpass",
		Certificates: certs,
	}
	client, e := rpcclient.New(connCfg, &ntfnHandlers, qu.T())
	if e != nil  {
		ftl.Ln(err)
	}
	// Get the list of unspent transaction outputs (utxos) that the connected wallet has at least one private key for.
	unspent, e := client.ListUnspent()
	if e != nil  {
		ftl.Ln(err)
	}
	log.Printf("Num unspent outputs (utxos): %d", len(unspent))
	if len(unspent) > 0 {
		log.Printf("First utxo:\n%v", spew.Sdump(unspent[0]))
	}
	// For this example gracefully shutdown the client after 10 seconds. Ordinarily when to shutdown the client is
	// highly application specific.
	fmt.Println("Client shutdown in 10 seconds...")
	time.AfterFunc(time.Second*10, func() {
		fmt.Println("Client shutting down...")
		client.Shutdown()
		fmt.Println("Client shutdown complete.")
	})
	// Wait until the client either shuts down gracefully (or the user terminates the process with Ctrl+C).
	client.WaitForShutdown()
}
