// +build ignore

package spv_test

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"github.com/p9c/pod/pkg/amt"
	"github.com/p9c/pod/pkg/btcaddr"
	"io"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	
	"github.com/p9c/pod/cmd/node/integration/rpctest"
	"github.com/p9c/pod/cmd/spv"
	"github.com/p9c/pod/pkg/btcjson"
	"github.com/p9c/pod/pkg/chainhash"
	ec "github.com/p9c/pod/pkg/ecc"
	"github.com/p9c/pod/pkg/gcs/builder"
	"github.com/p9c/pod/pkg/rpcclient"
	"github.com/p9c/pod/pkg/txauthor"
	"github.com/p9c/pod/pkg/txscript"
	"github.com/p9c/pod/pkg/util"
	"github.com/p9c/pod/pkg/waddrmgr"
	_ "github.com/p9c/pod/pkg/walletdb/bdb"
	"github.com/p9c/pod/pkg/wire"
)

var (
	// Try log.LevelInfo for output like you'd see in normal operation, or log.LevelTrace to help debug code. Anything
	// but log.LevelOff turns on log messages from the tests themselves as well. Keep in mind some log messages may not
	// appear in order due to use of multiple query goroutines in the tests.
	logLevel    = LevelOff
	syncTimeout = 30 * time.Second
	syncUpdate  = time.Second
	// Don't set this too high for your platform, or the tests will miss messages. TODO: Make this a benchmark instead.
	// TODO: Implement load limiting for both outgoing and incoming messages.
	numQueryThreads = 20
	queryOptions    = []spv.QueryOption{}
	// The logged sequence of events we want to see. The value of i represents the block for which a loop is generating
	// a log entry, given for readability only.
	//
	// "bc":	OnBlockConnected
	// "fc" xx:	OnFilteredBlockConnected with xx (uint8) relevant TXs
	// "rv":	OnRecvTx
	// "rd":	OnRedeemingTx
	// "bd":	OnBlockDisconnected
	// "fd":	OnFilteredBlockDisconnected
	wantLog = func() (log []byte) {
		for i := 1096; i <= 1100; i++ {
			// FilteredBlockConnected
			log = append(log, []byte("fc")...)
			// 0 relevant TXs
			log = append(log, 0x00)
			// BlockConnected
			log = append(log, []byte("bc")...)
		}
		// Block with one relevant (receive) transaction
		log = append(log, []byte("rvfc")...)
		log = append(log, 0x01)
		log = append(log, []byte("bc")...)
		// 124 blocks with nothing
		for i := 1102; i <= 1225; i++ {
			log = append(log, []byte("fc")...)
			log = append(log, 0x00)
			log = append(log, []byte("bc")...)
		}
		// Block with 1 redeeming transaction
		log = append(log, []byte("rdfc")...)
		log = append(log, 0x01)
		log = append(log, []byte("bc")...)
		// Block with nothing
		log = append(log, []byte("fc")...)
		log = append(log, 0x00)
		log = append(log, []byte("bc")...)
		// Update with rewind - rewind back to 1095, add another address, and see more interesting transactions.
		for i := 1227; i >= 1096; i-- {
			// BlockDisconnected and FilteredBlockDisconnected
			log = append(log, []byte("bdfd")...)
		}
		// Forward to 1100
		for i := 1096; i <= 1100; i++ {
			// FilteredBlockConnected
			log = append(log, []byte("fc")...)
			// 0 relevant TXs
			log = append(log, 0x00)
			// BlockConnected
			log = append(log, []byte("bc")...)
		}
		// Block with two relevant (receive) transactions
		log = append(log, []byte("rvrvfc")...)
		log = append(log, 0x02)
		log = append(log, []byte("bc")...)
		// 124 blocks with nothing
		for i := 1102; i <= 1225; i++ {
			log = append(log, []byte("fc")...)
			log = append(log, 0x00)
			log = append(log, []byte("bc")...)
		}
		// 2 blocks with 1 redeeming transaction each
		for i := 1226; i <= 1227; i++ {
			log = append(log, []byte("rdfc")...)
			log = append(log, 0x01)
			log = append(log, []byte("bc")...)
		}
		// Block with nothing
		log = append(log, []byte("fc")...)
		log = append(log, 0x00)
		log = append(log, []byte("bc")...)
		// 3 block rollback
		for i := 1228; i >= 1226; i-- {
			log = append(log, []byte("fdbd")...)
		}
		// 5 block empty reorg
		for i := 1226; i <= 1230; i++ {
			log = append(log, []byte("fc")...)
			log = append(log, 0x00)
			log = append(log, []byte("bc")...)
		}
		// 5 block rollback
		for i := 1230; i >= 1226; i-- {
			log = append(log, []byte("fdbd")...)
		}
		// 2 blocks with 1 redeeming transaction each
		for i := 1226; i <= 1227; i++ {
			log = append(log, []byte("rdfc")...)
			log = append(log, 0x01)
			log = append(log, []byte("bc")...)
		}
		// 8 block rest of reorg
		for i := 1228; i <= 1235; i++ {
			log = append(log, []byte("fc")...)
			log = append(log, 0x00)
			log = append(log, []byte("bc")...)
		}
		return log
	}()
	// rescanMtx locks all the variables to which the rescan goroutine's notifications write.
	rescanMtx sync.RWMutex
	// gotLog is where we accumulate the event log from the rescan. Then we compare it to wantLog to see if the series
	// of events the rescan saw happened as expected.
	gotLog []byte
	// curBlockHeight lets the rescan goroutine track where it thinks the chain is based on OnBlockConnected and
	// OnBlockDisconnected.
	curBlockHeight int32
	// curFilteredBlockHeight lets the rescan goroutine track where it thinks the chain is based on
	// OnFilteredBlockConnected and OnFilteredBlockDisconnected.
	curFilteredBlockHeight int32
	// ourKnownTxsByBlock lets the rescan goroutine keep track of transactions we're interested in that are in the
	// blockchain we're following as signalled by OnBlockConnected, OnBlockDisconnected, OnRecvTx, and OnRedeemingTx.
	ourKnownTxsByBlock = make(map[chainhash.Hash][]*util.Tx)
	// ourKnownTxsByFilteredBlock lets the rescan goroutine keep track of transactions we're interested in that are in
	// the blockchain we're following as signalled by OnFilteredBlockConnected and OnFilteredBlockDisconnected.
	ourKnownTxsByFilteredBlock = make(map[chainhash.Hash][]*util.Tx)
)

// secSource is an implementation of btcwallet/txauthor/SecretsSource that stores WitnessPubKeyHash addresses.
type secSource struct {
	keys    map[string]*ec.PrivateKey
	scripts map[string]*[]byte
	params  *chaincfg.Params
}

func (s *secSource) add(privKey *ec.PrivateKey) (btcaddr.Address, error) {
	pubKeyHash := btcaddr.Hash160(privKey.PubKey().SerializeCompressed())
	addr, e := util.NewAddressWitnessPubKeyHash(pubKeyHash, s.params)
	if e != nil {
		return nil, e
	}
	script, e := txscript.PayToAddrScript(addr)
	if e != nil {
		return nil, e
	}
	s.keys[addr.String()] = privKey
	s.scripts[addr.String()] = &script
	_, addrs, _, e = txscript.ExtractPkScriptAddrs(script, s.params)
	if e != nil {
		return nil, e
	}
	if addrs[0].String() != addr.String() {
		return nil, fmt.Errorf(
			"Encoded and decoded addresses don't "+
				"match. Encoded: %s, decoded: %s", addr, addrs[0],
		)
	}
	return addr, nil
}

// GetKey is required by the txscript.KeyDB interface
func (s *secSource) GetKey(addr btcaddr.Address) (
	*ec.PrivateKey, bool,
	error,
) {
	privKey, ok := s.keys[addr.String()]
	if !ok {
		return nil, true, fmt.Errorf("No key for address %s", addr)
	}
	return privKey, true, nil
}

// GetScript is required by the txscript.ScriptDB interface
func (s *secSource) GetScript(addr btcaddr.Address) ([]byte, error) {
	script, ok := s.scripts[addr.String()]
	if !ok {
		return nil, fmt.Errorf("No script for address %s", addr)
	}
	return *script, nil
}

// ChainParams is required by the SecretsSource interface
func (s *secSource) ChainParams() *chaincfg.Params {
	return s.params
}

func newSecSource(params *chaincfg.Params) *secSource {
	return &secSource{
		keys:    make(map[string]*ec.PrivateKey),
		scripts: make(map[string]*[]byte),
		params:  params,
	}
}

// type testLogger struct {
// 	//nolint
// 	t *testing.T
// }

type neutrinoHarness struct {
	h1, h2, h3 *rpctest.Harness
	svc        *spv.ChainService
}

type syncTestCase struct {
	name string
	test func(harness *neutrinoHarness, t *testing.T)
}

// var testCases = []*syncTestCase{
// 	{
// 		name: "initial sync",
// 		test: testInitialSync,
// 	},
// 	{
// 		name: "one-shot rescan",
// 		test: testRescan,
// 	},
// 	{
// 		name: "start long-running rescan",
// 		test: testStartRescan,
// 	},
// 	{
// 		name: "test blocks and filters in random order",
// 		test: testRandomBlocks,
// 	},
// 	{
// 		name: "check long-running rescan results",
// 		test: testRescanResults,
// 	},
// }

// Make sure the client synchronizes with the correct node.
func testInitialSync(harness *neutrinoHarness, t *testing.T) {
	e := waitForSync(t, harness.svc, harness.h1)
	if e != nil {
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
}

// Variables used to track state between multiple rescan tests.
var (
	quitRescan                qu.C
	errChan                   <-chan error
	rescan                    *spv.Rescan
	startBlock                waddrmgr.BlockStamp
	secSrc                    *secSource
	addr1, addr2, addr3       btcaddr.Address
	script1, script2, script3 []byte
	tx1, tx2, tx3             *wire.MsgTx
	ourOutPoint               wire.OutPoint
)

// testRescan tests several rescan modes. This should be broken up into smaller tests.
func testRescan(harness *neutrinoHarness, t *testing.T) {
	// Generate an address and send it some coins on the h1 chain. We use this to test rescans and notifications.
	modParams := harness.svc.ChainParams()
	secSrc = newSecSource(&modParams)
	privKey1, e := ec.NewPrivateKey(ec.S256())
	if e != nil {
		t.Fatalf("Couldn't generate private key: %s", err)
	}
	addr1, e = secSrc.add(privKey1)
	if e != nil {
		t.Fatalf("Couldn't create address from key: %s", err)
	}
	script1, e = secSrc.GetScript(addr1)
	if e != nil {
		t.Fatalf("Couldn't create script from address: %s", err)
	}
	out1 := wire.TxOut{
		PkScript: script1,
		Value:    1000000000,
	}
	// Fee rate is satoshis per byte
	tx1, e = harness.h1.CreateTransaction(
		[]*wire.TxOut{&out1}, 1000, true,
	)
	if e != nil {
		t.Fatalf("Couldn't create transaction from script: %s", err)
	}
	_, e = harness.h1.Node.SendRawTransaction(tx1, true)
	if e != nil {
		t.Fatalf("Unable to send raw transaction to node: %s", err)
	}
	privKey2, e := ec.NewPrivateKey(ec.S256())
	if e != nil {
		t.Fatalf("Couldn't generate private key: %s", err)
	}
	addr2, e = secSrc.add(privKey2)
	if e != nil {
		t.Fatalf("Couldn't create address from key: %s", err)
	}
	script2, e = secSrc.GetScript(addr2)
	if e != nil {
		t.Fatalf("Couldn't create script from address: %s", err)
	}
	out2 := wire.TxOut{
		PkScript: script2,
		Value:    1000000000,
	}
	// Fee rate is satoshis per byte
	tx2, e = harness.h1.CreateTransaction(
		[]*wire.TxOut{&out2}, 1000, true,
	)
	if e != nil {
		t.Fatalf("Couldn't create transaction from script: %s", err)
	}
	_, e = harness.h1.Node.SendRawTransaction(tx2, true)
	if e != nil {
		t.Fatalf("Unable to send raw transaction to node: %s", err)
	}
	_, e = harness.h1.Node.Generate(1)
	if e != nil {
		t.Fatalf("Couldn't generate/submit block: %s", err)
	}
	e = waitForSync(t, harness.svc, harness.h1)
	if e != nil {
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	// Call GetUtxo for our output in tx1 to see if it's spent.
	ourIndex := 1 << 30 // Should work on 32-bit systems
	for i, txo := range tx1.TxOut {
		if bytes.Equal(txo.PkScript, script1) {
			ourIndex = i
		}
	}
	if ourIndex != 1<<30 {
		ourOutPoint = wire.OutPoint{
			Hash:  tx1.TxHash(),
			Index: uint32(ourIndex),
		}
	} else {
		t.Fatalf(
			"Couldn't find the index of our output in transaction"+
				" %s", tx1.TxHash(),
		)
	}
	spendReport, e := harness.svc.GetUtxo(
		spv.WatchInputs(
			spv.InputWithScript{
				PkScript: script1,
				OutPoint: ourOutPoint,
			},
		),
		spv.StartBlock(&waddrmgr.BlockStamp{Height: 1101}),
	)
	if e != nil {
		t.Fatalf("Couldn't get UTXO %s: %s", ourOutPoint, err)
	}
	if !bytes.Equal(spendReport.Output.PkScript, script1) {
		t.Fatalf(
			"UTXO's script doesn't match expected script for %s",
			ourOutPoint,
		)
	}
}

func testStartRescan(harness *neutrinoHarness, t *testing.T) {
	// Start a rescan with notifications in another goroutine. We'll kill it with a quit channel at the end and make
	// sure we got the expected results.
	quitRescan = qu.T()
	startBlock = waddrmgr.BlockStamp{Height: 1095}
	rescan, errChan = startRescan(
		t, harness.svc, addr1, &startBlock,
		quitRescan,
	)
	e := waitForSync(t, harness.svc, harness.h1)
	if e != nil {
		checkErrChan(t, errChan)
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	numTXs, _, e = checkRescanStatus()
	if e != nil {
		checkErrChan(t, errChan)
		t.Fatalf("Checking rescan status failed: %s", err)
	}
	if numTXs != 1 {
		t.Fatalf(
			"Wrong number of relevant transactions. Want: 1, got:"+
				" %d", numTXs,
		)
	}
	// Generate 124 blocks on h1 to make sure it reorgs the other nodes. Ensure the ChainService instance stays caught
	// up.
	_, e = harness.h1.Node.Generate(124)
	if e != nil {
		t.Log(e)
	}
	e = waitForSync(t, harness.svc, harness.h1)
	if e != nil {
		checkErrChan(t, errChan)
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	// Connect/sync/disconnect h2 to make it reorg to the h1 chain.
	e = csd([]*rpctest.Harness{harness.h1, harness.h2})
	if e != nil {
		checkErrChan(t, errChan)
		t.Fatalf("Couldn't sync h2 to h1: %s", err)
	}
	// Spend the outputs we sent ourselves over two blocks.
	inSrc := func(tx wire.MsgTx) func(target amt.Amount) (
		total amt.Amount, inputs []*wire.TxIn,
		inputValues []amt.Amount, scripts [][]byte, e error,
	) {
		ourIndex := 1 << 30 // Should work on 32-bit systems
		for i, txo := range tx.TxOut {
			if bytes.Equal(txo.PkScript, script1) ||
				bytes.Equal(txo.PkScript, script2) {
				ourIndex = i
			}
		}
		return func(target amt.Amount) (
			total amt.Amount,
			inputs []*wire.TxIn, inputValues []amt.Amount,
			scripts [][]byte, e error,
		) {
			if ourIndex == 1<<30 {
				e = fmt.Errorf(
					"Couldn't find our address " +
						"in the passed transaction's outputs.",
				)
				return
			}
			total = target
			inputs = []*wire.TxIn{
				{
					PreviousOutPoint: wire.OutPoint{
						Hash:  tx.TxHash(),
						Index: uint32(ourIndex),
					},
				},
			}
			inputValues = []amt.Amount{
				amt.Amount(tx.TxOut[ourIndex].Value),
			}
			scripts = [][]byte{tx.TxOut[ourIndex].PkScript}
			e = nil
			return
		}
	}
	// Create another address to send to so we don't trip the rescan with the old address and we can test monitoring
	// both OutPoint usage and receipt by addresses.
	privKey3, e := ec.NewPrivateKey(ec.S256())
	if e != nil {
		t.Fatalf("Couldn't generate private key: %s", err)
	}
	addr3, e = secSrc.add(privKey3)
	if e != nil {
		t.Fatalf("Couldn't create address from key: %s", err)
	}
	script3, e = secSrc.GetScript(addr3)
	if e != nil {
		t.Fatalf("Couldn't create script from address: %s", err)
	}
	out3 := wire.TxOut{
		PkScript: script3,
		Value:    500000000,
	}
	// Spend the first transaction and mine a block.
	authTx1, e := txauthor.NewUnsignedTransaction(
		[]*wire.TxOut{
			&out3,
		},
		// Fee rate is satoshis per kilobyte
		1024000,
		inSrc(*tx1),
		func() ([]byte, error) {
			return script3, nil
		},
	)
	if e != nil {
		t.Fatalf("Couldn't create unsigned transaction: %s", err)
	}
	e = authTx1.AddAllInputScripts(secSrc)
	if e != nil {
		t.Fatalf("Couldn't sign transaction: %s", err)
	}
	banPeer(harness.svc, harness.h2)
	e = harness.svc.SendTransaction(
		authTx1.Tx,
		append(
			queryOptions,
			spv.PeerConnectTimeout(3*time.Second),
		)...,
	)
	if e != nil && !strings.Contains(err.Error(), "already have") {
		t.Fatalf("Unable to send transaction to network: %s", err)
	}
	_, e = harness.h1.Node.Generate(1)
	if e != nil {
		t.Fatalf("Couldn't generate/submit block: %s", err)
	}
	e = waitForSync(t, harness.svc, harness.h1)
	if e != nil {
		checkErrChan(t, errChan)
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	numTXs, _, e = checkRescanStatus()
	if numTXs != 2 {
		t.Fatalf(
			"Wrong number of relevant transactions. Want: 2, got:"+
				" %d", numTXs,
		)
	}
	// Spend the second transaction and mine a block.
	authTx2, e := txauthor.NewUnsignedTransaction(
		[]*wire.TxOut{
			&out3,
		},
		// Fee rate is satoshis per kilobyte
		1024000,
		inSrc(*tx2),
		func() ([]byte, error) {
			return script3, nil
		},
	)
	if e != nil {
		t.Fatalf("Couldn't create unsigned transaction: %s", err)
	}
	e = authTx2.AddAllInputScripts(secSrc)
	if e != nil {
		t.Fatalf("Couldn't sign transaction: %s", err)
	}
	banPeer(harness.svc, harness.h2)
	e = harness.svc.SendTransaction(
		authTx2.Tx,
		append(
			queryOptions,
			spv.PeerConnectTimeout(3*time.Second),
		)...,
	)
	if e != nil && !strings.Contains(err.Error(), "already have") {
		t.Fatalf("Unable to send transaction to network: %s", err)
	}
	_, e = harness.h1.Node.Generate(1)
	if e != nil {
		t.Fatalf("Couldn't generate/submit block: %s", err)
	}
	e = waitForSync(t, harness.svc, harness.h1)
	if e != nil {
		checkErrChan(t, errChan)
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	numTXs, _, e = checkRescanStatus()
	if numTXs != 2 {
		t.Fatalf(
			"Wrong number of relevant transactions. Want: 2, got:"+
				" %d", numTXs,
		)
	}
	// Update the filter with the second address, and we should have 2 more relevant transactions.
	e = rescan.Update(spv.AddAddrs(addr2), spv.Rewind(1095))
	if e != nil {
		t.Fatalf("Couldn't update the rescan filter: %s", err)
	}
	e = waitForSync(t, harness.svc, harness.h1)
	if e != nil {
		checkErrChan(t, errChan)
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	numTXs, _, e = checkRescanStatus()
	if numTXs != 4 {
		t.Fatalf(
			"Wrong number of relevant transactions. Want: 4, got:"+
				" %d", numTXs,
		)
	}
	// Generate a block with a nonstandard coinbase to generate a basic filter with 0 entries.
	_, e = harness.h1.GenerateAndSubmitBlockWithCustomCoinbaseOutputs(
		[]*util.Tx{}, rpctest.BlockVersion, time.Time{},
		[]wire.TxOut{
			{
				Value:    0,
				PkScript: []byte{},
			},
		},
	)
	if e != nil {
		t.Fatalf("Couldn't generate/submit block: %s", err)
	}
	e = waitForSync(t, harness.svc, harness.h1)
	if e != nil {
		checkErrChan(t, errChan)
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	// Chk and make sure the previous UTXO is now spent.
	spendReport, e := harness.svc.GetUtxo(
		spv.WatchInputs(
			spv.InputWithScript{
				PkScript: script1,
				OutPoint: ourOutPoint,
			},
		),
		spv.StartBlock(&waddrmgr.BlockStamp{Height: 801}),
	)
	if e != nil {
		t.Fatalf("Couldn't get UTXO %s: %s", ourOutPoint, err)
	}
	if spendReport.SpendingTx == nil {
		t.Fatalf("Unable to find initial transaction")
	}
	if spendReport.SpendingTx.TxHash() != authTx1.Tx.TxHash() {
		t.Fatalf(
			"Redeeming transaction doesn't match expected "+
				"transaction: want %s, got %s", authTx1.Tx.TxHash(),
			spendReport.SpendingTx.TxHash(),
		)
	}
}

func fetchPrevInputScripts(block *wire.Block, client *rpctest.Harness) ([][]byte, error) {
	var inputScripts [][]byte
	for i, tx := range block.Transactions {
		if i == 0 {
			continue
		}
		for _, txIn := range tx.TxIn {
			prevTxHash := txIn.PreviousOutPoint.Hash
			prevTx, e := client.Node.GetRawTransaction(&prevTxHash)
			if e != nil {
				return nil, e
			}
			prevIndex := txIn.PreviousOutPoint.Index
			prevOutput := prevTx.MsgTx().TxOut[prevIndex]
			inputScripts = append(inputScripts, prevOutput.PkScript)
		}
	}
	return inputScripts, nil
}

func testRescanResults(harness *neutrinoHarness, t *testing.T) {
	// Generate 5 blocks on h2 and wait for ChainService to sync to the newly-best chain on h2. This includes the
	// transactions sent via svc.SendTransaction earlier, so we'll have to check that the rescan status has updated for
	// the correct number of transactions.
	_, e := harness.h2.Node.Generate(5)
	if e != nil {
		t.Fatalf("Couldn't generate/submit blocks: %s", err)
	}
	e = waitForSync(t, harness.svc, harness.h2)
	if e != nil {
		checkErrChan(t, errChan)
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	numTXs, _, e = checkRescanStatus()
	if numTXs != 2 {
		t.Fatalf(
			"Wrong number of relevant transactions. Want: 2, got:"+
				" %d", numTXs,
		)
	}
	// Generate 7 blocks on h1 and wait for ChainService to sync to the newly-best chain on h1.
	_, e = harness.h1.Node.Generate(7)
	if e != nil {
		t.Fatalf("Couldn't generate/submit block: %s", err)
	}
	e = waitForSync(t, harness.svc, harness.h1)
	if e != nil {
		checkErrChan(t, errChan)
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	numTXs, _, e = checkRescanStatus()
	if numTXs != 4 {
		t.Fatalf(
			"Wrong number of relevant transactions. Want: 4, got:"+
				" %d", numTXs,
		)
	}
	if !bytes.Equal(wantLog, gotLog) {
		leastBytes := len(wantLog)
		if len(gotLog) < leastBytes {
			leastBytes = len(gotLog)
		}
		diffIndex := 0
		for i := 0; i < leastBytes; i++ {
			if wantLog[i] != gotLog[i] {
				diffIndex = i
				break
			}
		}
		t.Fatalf(
			"Rescan event logs differ starting at %d.\nWant: %v\n"+
				"Got:  %v\nDifference - want: %v\nDifference -- got: "+
				"%v", diffIndex, wantLog, gotLog, wantLog[diffIndex:],
			gotLog[diffIndex:],
		)
	}
	// Connect h1 and h2, wait for them to synchronize and check for the ChainService synchronization status.
	e = rpctest.ConnectNode(harness.h1, harness.h2)
	if e != nil {
		t.Fatalf("Couldn't connect h1 to h2: %s", err)
	}
	e = rpctest.JoinNodes(
		[]*rpctest.Harness{harness.h1, harness.h2},
		rpctest.Blocks,
	)
	if e != nil {
		t.Fatalf("Couldn't sync h1 and h2: %s", err)
	}
	e = waitForSync(t, harness.svc, harness.h1)
	if e != nil {
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	// Now generate a bunch of blocks on each while they're connected, triggering many tiny reorgs, and wait for sync
	// again. The end result is somewhat random, depending on how quickly the nodes process each other's notifications
	// vs finding new blocks, but the two nodes should remain fully synchronized with each other at the end.
	go harness.h2.Node.Generate(75)
	harness.h1.Node.Generate(50)
	e = rpctest.JoinNodes(
		[]*rpctest.Harness{harness.h1, harness.h2},
		rpctest.Blocks,
	)
	if e != nil {
		t.Fatalf("Couldn't sync h1 and h2: %s", err)
	}
	// We increase the timeout because running on Travis with race detection enabled can make this pretty slow.
	syncTimeout *= 2
	e = waitForSync(t, harness.svc, harness.h1)
	if e != nil {
		checkErrChan(t, errChan)
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	quitRescan.Q()
	e = <-errChan
	quitRescan = nil
	if err != spv.ErrRescanExit {
		t.Fatalf("Rescan ended with error: %s", err)
	}
	// Immediately try to add a new update to to the rescan that was just shut down. This should fail as it is no longer
	// running.
	rescan.WaitForShutdown()
	e = rescan.Update(spv.AddAddrs(addr2), spv.Rewind(1095))
	if e == nil {
		t.Fatalf("Expected update call to fail, it did not")
	}
}

// testRandomBlocks goes through all blocks in random order and ensures we can correctly get cfilters from them. It uses
// numQueryThreads goroutines running at the same time to go through this. 50 is comfortable on my somewhat dated laptop
// with default query optimization settings. TODO: Make this a benchmark instead.
func testRandomBlocks(harness *neutrinoHarness, t *testing.T) {
	var haveBest *waddrmgr.BlockStamp
	haveBest, e := harness.svc.BestBlock()
	if e != nil {
		t.Fatalf("Couldn't get best snapshot from ChainService: %s", err)
	}
	// Keep track of an error channel with enough buffer space to track one error per block.
	errChan := make(chan error, haveBest.Height)
	// Test getting all of the blocks and filters.
	var wg sync.WaitGroup
	workerQueue := qu.T(numQueryThreads)
	for i := int32(1); i <= haveBest.Height; i++ {
		wg.Add(1)
		height := uint32(i)
		// Wait until there's room in the worker queue.
		workerQueue <- struct{}{}
		go func() {
			// On exit, open a spot in workerQueue and tell the wait group we're done.
			defer func() {
				<-workerQueue
			}()
			defer wg.Done()
			// Get block header from database.
			blockHeader, e := harness.svc.BlockHeaders.
				FetchHeaderByHeight(height)
			if e != nil {
				errChan <- fmt.Errorf(
					"Couldn't get block "+
						"header by height %d: %s", height, e,
				)
				return
			}
			blockHash := blockHeader.BlockHash()
			// Get block via RPC.
			wantBlock, e := harness.h1.Node.GetBlock(&blockHash)
			if e != nil {
				errChan <- fmt.Errorf(
					"Couldn't get block %d "+
						"(%s) by RPC", height, blockHash,
				)
				return
			}
			// Get block from network.
			haveBlock, e := harness.svc.GetBlock(
				blockHash, queryOptions...,
			)
			if e != nil {
				errChan <- err
				return
			}
			if haveBlock == nil {
				errChan <- fmt.Errorf(
					"Couldn't get block %d "+
						"(%s) from network", height, blockHash,
				)
				return
			}
			// Chk that network and RPC blocks match.
			if !reflect.DeepEqual(
				*haveBlock.MsgBlock(),
				*wantBlock,
			) {
				errChan <- fmt.Errorf(
					"Block from network "+
						"doesn't match block from RPC. Want: "+
						"%s, RPC: %s, network: %s", blockHash,
					wantBlock.BlockHash(),
					haveBlock.MsgBlock().BlockHash(),
				)
				return
			}
			// Chk that block height matches what we have.
			if height != uint32(haveBlock.Height()) {
				errChan <- fmt.Errorf(
					"Block height from "+
						"network doesn't match expected "+
						"height. Want: %v, network: %v",
					height, haveBlock.Height(),
				)
				return
			}
			// Get basic cfilter from network.
			haveFilter, e := harness.svc.GetCFilter(
				blockHash,
				wire.GCSFilterRegular, queryOptions...,
			)
			if e != nil {
				errChan <- err
				return
			}
			// Get basic cfilter from RPC.
			wantFilter, e := harness.h1.Node.GetCFilter(
				&blockHash, wire.GCSFilterRegular,
			)
			if e != nil {
				errChan <- fmt.Errorf(
					"Couldn't get basic "+
						"filter for block %d (%s) via RPC: %s",
					height, blockHash, e,
				)
				return
			}
			// Chk that network and RPC cfilters match.
			var haveBytes []byte
			if haveFilter != nil {
				haveBytes, e = haveFilter.NBytes()
				if e != nil {
					errChan <- fmt.Errorf(
						"Couldn't get "+
							"basic filter for block %d "+
							"(%s) via P2P: %s", height,
						blockHash, err,
					)
					return
				}
			}
			if !bytes.Equal(haveBytes, wantFilter.Data) {
				errChan <- fmt.Errorf(
					"Basic filter from P2P "+
						"network/DB doesn't match RPC value "+
						"for block %d (%s):\nRPC: %s\nNet: %s",
					height, blockHash,
					hex.EncodeToString(wantFilter.Data),
					hex.EncodeToString(haveBytes),
				)
				return
			}
			inputScripts, e := fetchPrevInputScripts(
				haveBlock.MsgBlock(),
				harness.h1,
			)
			if e != nil {
				errChan <- fmt.Errorf(
					"unable to create prev "+
						"input scripts: %v", err,
				)
				return
			}
			// Calculate basic filter from block.
			calcFilter, e := builder.BuildBasicFilter(
				haveBlock.MsgBlock(), inputScripts,
			)
			if e != nil {
				errChan <- fmt.Errorf(
					"Couldn't build basic "+
						"filter for block %d (%s): %s", height,
					blockHash, err,
				)
				return
			}
			calcBytes, e := calcFilter.NBytes()
			if e != nil {
				errChan <- fmt.Errorf(
					"Couldn't get bytes from"+
						" calculated basic filter for block "+
						"%d (%s): %s", height, blockHash, err,
				)
			}
			// Chk that the network value matches the calculated value from the block.
			if !bytes.Equal(haveBytes, calcBytes) {
				errChan <- fmt.Errorf(
					"Basic filter from P2P "+
						"network/DB doesn't match calculated "+
						"value for block %d (%s)", height,
					blockHash,
				)
				return
			}
			// Get previous basic filter header from the database.
			prevHeader, e := harness.svc.RegFilterHeaders.
				FetchHeader(&blockHeader.PrevBlock)
			if e != nil {
				errChan <- fmt.Errorf(
					"Couldn't get basic "+
						"filter header for block %d (%s) from "+
						"DB: %s", height-1,
					blockHeader.PrevBlock, err,
				)
				return
			}
			// Get current basic filter header from the database.
			curHeader, e := harness.svc.RegFilterHeaders.
				FetchHeader(&blockHash)
			if e != nil {
				errChan <- fmt.Errorf(
					"Couldn't get basic "+
						"filter header for block %d (%s) from "+
						"DB: %s", height, blockHash, err,
				)
				return
			}
			// Chk that the filter and header line up.
			calcHeader, e := builder.MakeHeaderForFilter(
				calcFilter, *prevHeader,
			)
			if e != nil {
				errChan <- fmt.Errorf(
					"Couldn't calculate "+
						"header for basic filter for block "+
						"%d (%s): %s", height, blockHash, err,
				)
				return
			}
			if !bytes.Equal(curHeader[:], calcHeader[:]) {
				errChan <- fmt.Errorf(
					"Filter header doesn't "+
						"match. Want: %s, got: %s", curHeader,
					calcHeader,
				)
				return
			}
		}()
	}
	// Wait for all queries to finish.
	wg.Wait()
	// Close the error channel to make the error monitoring goroutine finish.
	close(errChan)
	var lastErr error
	for e := range errChan {
		if e != nil {
			t.Errorf("%s", err)
			lastErr = fmt.Errorf(
				"Couldn't validate all " +
					"blocks, filters, and filter headers.",
			)
		}
	}
	if logLevel != LevelOff {
		t.Logf(
			"Finished checking %d blocks and their cfilters",
			haveBest.Height,
		)
	}
	if lastErr != nil {
		t.Fatal(lastErr)
	}
}

// func TestNeutrinoSync(// 	t *testing.T) {
// 	// Set up logging.
// 	logger := log.NewBackend(os.Out)
// 	chainLogger := logger.Logger("CHAIN")
// 	chainLogger.SetLevel(logLevel)
// 	// spv.UseLogger(chainLogger)
// 	rpcLogger := logger.Logger("RPCC")
// 	rpcLogger.SetLevel(logLevel)
// 	// rpcclient.UseLogger(rpcLogger)
// 	// Create a btcd SimNet node and generate 800 blocks
// 	h1, e := rpctest.New(
// 		&chaincfg.SimNetParams, nil, []string{"--txindex"},
// 	)
// 	if e != nil  {
// 		t.Fatalf("Couldn't create harness: %s", err)
// 	}
// 	defer h1.TearDown()
// 	e = h1.SetUp(false, 0)
// 	if e != nil  {
// 		t.Fatalf("Couldn't set up harness: %s", err)
// 	}
// 	_, e = h1.Node.Generate(800)
// 	if e != nil  {
// 		t.Fatalf("Couldn't generate blocks: %s", err)
// 	}
// 	// Create a second btcd SimNet node
// 	h2, e := rpctest.New(
// 		&chaincfg.SimNetParams, nil, []string{"--txindex"},
// 	)
// 	if e != nil  {
// 		t.Fatalf("Couldn't create harness: %s", err)
// 	}
// 	defer h2.TearDown()
// 	e = h2.SetUp(false, 0)
// 	if e != nil  {
// 		t.Fatalf("Couldn't set up harness: %s", err)
// 	}
// 	// Create a third btcd SimNet node and generate 1200 blocks
// 	h3, e := rpctest.New(
// 		&chaincfg.SimNetParams, nil, []string{"--txindex"},
// 	)
// 	if e != nil  {
// 		t.Fatalf("Couldn't create harness: %s", err)
// 	}
// 	defer h3.TearDown()
// 	e = h3.SetUp(false, 0)
// 	if e != nil  {
// 		t.Fatalf("Couldn't set up harness: %s", err)
// 	}
// 	_, e = h3.Node.Generate(1200)
// 	if e != nil  {
// 		t.Fatalf("Couldn't generate blocks: %s", err)
// 	}
// 	// Connect, sync, and disconnect h1 and h2
// 	e = csd([]*rpctest.Harness{h1, h2})
// 	if e != nil  {
// 		t.Fatalf("Couldn't connect/sync/disconnect h1 and h2: %s", err)
// 	}
// 	// Generate 300 blocks on the first node and 350 on the second
// 	_, e = h1.Node.Generate(300)
// 	if e != nil  {
// 		t.Fatalf("Couldn't generate blocks: %s", err)
// 	}
// 	_, e = h2.Node.Generate(350)
// 	if e != nil  {
// 		t.Fatalf("Couldn't generate blocks: %s", err)
// 	}
// 	// Now we have a node with 1100 blocks (h1), 1150 blocks (h2), and
// 	// 1200 blocks (h3). The chains of nodes h1 and h2 match up to block
// 	// 800. By default, a synchronizing wallet connected to all three
// 	// should synchronize to h3. However, we're going to take checkpoints
// 	// from h1 at 111, 333, 555, 777, and 999, and add those to the
// 	// synchronizing wallet's chain parameters so that it should
// 	// disconnect from h3 at block 111, and from h2 at block 555, and
// 	// then synchronize to block 800 from h1. Order of connection is
// 	// unfortunately not guaranteed, so the reorg may not happen with every
// 	// test.
// 	// Copy parameters and insert checkpoints
// 	modParams := chaincfg.SimNetParams
// 	for _, height := range []int64{111, 333, 555, 777, 999} {
// 		hash, e := h1.Node.GetBlockHash(height)
// 		if e != nil  {
// 			t.Fatalf("Couldn't get block hash for height %d: %s",
// 				height, err)
// 		}
// 		modParams.Checkpoints = append(modParams.Checkpoints,
// 			chaincfg.Checkpoint{
// 				Hash:   hash,
// 				Height: int32(height),
// 			})
// 	}
// 	// Create a temporary directory, initialize an empty walletdb with an
// 	// SPV chain namespace, and create a configuration for the ChainService.
// 	tempDir, e := ioutil.TempDir("", "neutrino")
// 	if e != nil  {
// 		t.Fatalf("Failed to create temporary directory: %s", err)
// 	}
// 	defer os.RemoveAll(tempDir)
// 	db, e := walletdb.Create("bdb", tempDir+"/weks.db")
// 	defer db.Close()
// 	if e != nil  {
// 		t.Fatalf("ScriptError opening DB: %s\n", err)
// 	}
// 	config := spv.Config{
// 		DataDir:     tempDir,
// 		Database:    db,
// 		ChainParams: modParams,
// 		AddPeers: []string{
// 			h3.P2PAddress(),
// 			h2.P2PAddress(),
// 			h1.P2PAddress(),
// 		},
// 	}
// 	spv.MaxPeers = 3
// 	spv.BanDuration = 5 * time.Second
// 	spv.QueryPeerConnectTimeout = 10 * time.Second
// 	svc, e := spv.NewChainService(config)
// 	if e != nil  {
// 		t.Fatalf("ScriptError creating ChainService: %s", err)
// 	}
// 	svc.Start()
// 	defer svc.Stop()
// 	// Create a test harness with the three nodes and the neutrino instance.
// 	testHarness := &neutrinoHarness{h1, h2, h3, svc}
// 	for _, test := range testCases {
// 		t.Run(test.name, func(t *testing.T) {
// 			test.test(testHarness, t)
// 		})
// 	}
// }

// csd does a connect-sync-disconnect between nodes in order to support
// reorg testing. It brings up and tears down a temporary node, otherwise the
// nodes try to reconnect to each other which results in unintended reorgs.
func csd(harnesses []*rpctest.Harness) (e error) {
	hTemp, e := rpctest.New(&chaincfg.SimNetParams, nil, nil)
	if e != nil {
		return e
	}
	// Tear down node at the end of the function.
	defer hTemp.TearDown()
	e = hTemp.SetUp(false, 0)
	if e != nil {
		return e
	}
	for _, harness := range harnesses {
		e = rpctest.ConnectNode(hTemp, harness)
		if e != nil {
			return e
		}
	}
	return rpctest.JoinNodes(harnesses, rpctest.Blocks)
}

// checkErrChan tries to read the passed error channel if possible and logs the error it found, if any. This is useful
// to help troubleshoot any timeouts during a rescan.
func checkErrChan(t *testing.T, errChan <-chan error) {
	select {
	case e := <-errChan:
		t.Logf("Got error from rescan: %s", err)
	default:
	}
}

// waitForSync waits for the ChainService to sync to the current chain state.
func waitForSync(
	t *testing.T, svc *spv.ChainService,
	correctSyncNode *rpctest.Harness,
) (e error) {
	knownBestHash, knownBestHeight, err :=
		correctSyncNode.Node.GetBestBlock()
	if e != nil {
		return e
	}
	if logLevel != LevelOff {
		t.Logf("Syncing to %d (%s)", knownBestHeight, knownBestHash)
	}
	var haveBest *waddrmgr.BlockStamp
	haveBest, e = svc.BestBlock()
	if e != nil {
		return fmt.Errorf(
			"Couldn't get best snapshot from "+
				"ChainService: %s", err,
		)
	}
	var total time.Duration
	for haveBest.Hash != *knownBestHash {
		if total > syncTimeout {
			return fmt.Errorf(
				"Timed out after %v waiting for "+
					"header synchronization.\n%s", syncTimeout,
				goroutineDump(),
			)
		}
		if haveBest.Height > knownBestHeight {
			return fmt.Errorf("synchronized to the wrong chain")
		}
		time.Sleep(syncUpdate)
		total += syncUpdate
		haveBest, e = svc.BestBlock()
		if e != nil {
			return fmt.Errorf(
				"Couldn't get best snapshot from "+
					"ChainService: %s", err,
			)
		}
	}
	// Chk if we're current.
	if !svc.IsCurrent() {
		return fmt.Errorf(
			"the ChainService doesn't see itself as " +
				"current",
		)
	}
	// Chk if we have all of the cfheaders.
	knownBasicHeader, e := correctSyncNode.Node.GetCFilterHeader(
		knownBestHash, wire.GCSFilterRegular,
	)
	if e != nil {
		return fmt.Errorf(
			"Couldn't get latest basic header from "+
				"%s: %s", correctSyncNode.P2PAddress(), err,
		)
	}
	haveBasicHeader := &chainhash.Hash{}
	for knownBasicHeader.PrevFilterHeader != *haveBasicHeader {
		if total > syncTimeout {
			return fmt.Errorf(
				"Timed out after %v waiting for "+
					"cfheaders synchronization.\n%s", syncTimeout,
				goroutineDump(),
			)
		}
		haveBasicHeader, e = svc.RegFilterHeaders.FetchHeader(knownBestHash)
		if e != nil {
			if e == io.EOF {
				haveBasicHeader = &chainhash.Hash{}
				time.Sleep(syncUpdate)
				total += syncUpdate
				continue
			}
			return fmt.Errorf(
				"Couldn't get regular filter header"+
					" for %s: %s", knownBestHash, err,
			)
		}
		time.Sleep(syncUpdate)
		total += syncUpdate
	}
	if logLevel != LevelOff {
		t.Logf(
			"Synced cfheaders to %d (%s)", haveBest.Height,
			haveBest.Hash,
		)
	}
	// At this point, we know we have good cfheaders. Now we wait for the rescan, if one is going, to catch up.
	for {
		if total > syncTimeout {
			return fmt.Errorf(
				"Timed out after %v waiting for "+
					"rescan to catch up.\n%s", syncTimeout,
				goroutineDump(),
			)
		}
		time.Sleep(syncUpdate)
		total += syncUpdate
		rescanMtx.RLock()
		// We don't want to do this if we haven't started a rescan yet.
		if len(gotLog) == 0 {
			rescanMtx.RUnlock()
			break
		}
		_, rescanHeight, e := checkRescanStatus()
		if e != nil {
			// If there's an error, that means the FilteredBlockConnected notifications are still catching up to the
			// BlockConnected notifications.
			rescanMtx.RUnlock()
			continue
		}
		if logLevel != LevelOff {
			t.Logf("Rescan caught up to block %d", rescanHeight)
		}
		if rescanHeight == haveBest.Height {
			rescanMtx.RUnlock()
			break
		}
		rescanMtx.RUnlock()
	}
	// At this point, we know the latest cfheader is stored in the ChainService database. We now compare each cfheader
	// the harness knows about to what's stored in the ChainService database to see if we've missed anything or messed
	// anything up.
	for i := int32(0); i <= haveBest.Height; i++ {
		if total > syncTimeout {
			return fmt.Errorf(
				"Timed out after %v waiting for "+
					"cfheaders DB to catch up.\n%s", syncTimeout,
				goroutineDump(),
			)
		}
		head, e := svc.BlockHeaders.FetchHeaderByHeight(uint32(i))
		if e != nil {
			return fmt.Errorf(
				"Couldn't read block by "+
					"height: %s", err,
			)
		}
		hash := head.BlockHash()
		haveBasicHeader, e = svc.RegFilterHeaders.FetchHeader(&hash)
		if e == io.EOF {
			// This sometimes happens due to reorgs after the service decides it's current. Just wait for the DB to
			// catch up and try again.
			time.Sleep(syncUpdate)
			total += syncUpdate
			i--
			continue
		}
		if e != nil {
			return fmt.Errorf(
				"Couldn't get basic header "+
					"for %d (%s) from DB: %v\n%s", i, hash,
				e, goroutineDump(),
			)
		}
		knownBasicHeader, e = correctSyncNode.Node.GetCFilterHeader(
			&hash, wire.GCSFilterRegular,
		)
		if e != nil {
			return fmt.Errorf(
				"Couldn't get basic header "+
					"for %d (%s) from node %s", i, hash,
				correctSyncNode.P2PAddress(),
			)
		}
		if *haveBasicHeader != knownBasicHeader.PrevFilterHeader {
			return fmt.Errorf(
				"Basic header for %d (%s) "+
					"doesn't match node %s. DB: %s, node: %s", i,
				hash, correctSyncNode.P2PAddress(),
				haveBasicHeader,
				knownBasicHeader.PrevFilterHeader,
			)
		}
	}
	return nil
}

// startRescan starts a rescan in another goroutine, and logs all notifications from the rescan. At the end, the log
// should match one we precomputed based on the flow of the test. The rescan starts at the genesis block and the
// notifications continue until the `quit` channel is closed.
func startRescan(
	t *testing.T, svc *spv.ChainService, addr btcaddr.Address,
	startBlock *waddrmgr.BlockStamp, quit qu.C,
) (
	*spv.Rescan, <-chan error,
) {
	rescan := svc.NewRescan(
		spv.QuitChan(quit),
		spv.WatchAddrs(addr),
		spv.StartBlock(startBlock),
		spv.NotificationHandlers(
			rpcclient.NotificationHandlers{
				OnBlockConnected: func(
					hash *chainhash.Hash,
					height int32, time time.Time,
				) {
					rescanMtx.Lock()
					gotLog = append(
						gotLog,
						[]byte("bc")...,
					)
					curBlockHeight = height
					rescanMtx.Unlock()
				},
				OnBlockDisconnected: func(
					hash *chainhash.Hash,
					height int32, time time.Time,
				) {
					rescanMtx.Lock()
					delete(ourKnownTxsByBlock, *hash)
					gotLog = append(
						gotLog,
						[]byte("bd")...,
					)
					curBlockHeight = height - 1
					rescanMtx.Unlock()
				},
				OnRecvTx: func(
					tx *util.Tx,
					details *btcjson.BlockDetails,
				) {
					rescanMtx.Lock()
					hash, e := chainhash.
						NewHashFromStr(
							details.Hash,
						)
					if e != nil {
						t.Errorf(
							"Couldn't "+
								"decode hash "+
								"%s: %s",
							details.Hash,
							e,
						)
					}
					ourKnownTxsByBlock[*hash] = append(
						ourKnownTxsByBlock[*hash],
						tx,
					)
					gotLog = append(
						gotLog,
						[]byte("rv")...,
					)
					rescanMtx.Unlock()
				},
				OnRedeemingTx: func(
					tx *util.Tx,
					details *btcjson.BlockDetails,
				) {
					rescanMtx.Lock()
					hash, e := chainhash.
						NewHashFromStr(
							details.Hash,
						)
					if e != nil {
						t.Errorf(
							"Couldn't "+
								"decode hash "+
								"%s: %s",
							details.Hash,
							e,
						)
					}
					ourKnownTxsByBlock[*hash] = append(
						ourKnownTxsByBlock[*hash],
						tx,
					)
					gotLog = append(
						gotLog,
						[]byte("rd")...,
					)
					rescanMtx.Unlock()
				},
				OnFilteredBlockConnected: func(
					height int32,
					header *wire.BlockHeader,
					relevantTxs []*util.Tx,
				) {
					rescanMtx.Lock()
					ourKnownTxsByFilteredBlock[header.BlockHash()] =
						relevantTxs
					gotLog = append(
						gotLog,
						[]byte("fc")...,
					)
					gotLog = append(
						gotLog,
						uint8(len(relevantTxs)),
					)
					curFilteredBlockHeight = height
					rescanMtx.Unlock()
				},
				OnFilteredBlockDisconnected: func(
					height int32,
					header *wire.BlockHeader,
				) {
					rescanMtx.Lock()
					delete(
						ourKnownTxsByFilteredBlock,
						header.BlockHash(),
					)
					gotLog = append(
						gotLog,
						[]byte("fd")...,
					)
					curFilteredBlockHeight =
						height - 1
					rescanMtx.Unlock()
				},
			},
		),
	)
	errChan := rescan.Start()
	return rescan, errChan
}

// checkRescanStatus returns the number of relevant transactions we currently know about and the currently known height.
func checkRescanStatus() (int, int32, error) {
	var txCount [2]int
	rescanMtx.RLock()
	defer rescanMtx.RUnlock()
	for _, list := range ourKnownTxsByBlock {
		for range list {
			txCount[0]++
		}
	}
	for _, list := range ourKnownTxsByFilteredBlock {
		for range list {
			txCount[1]++
		}
	}
	if txCount[0] != txCount[1] {
		return 0, 0, fmt.Errorf(
			"Conflicting transaction count " +
				"between notifications.",
		)
	}
	if curBlockHeight != curFilteredBlockHeight {
		return 0, 0, fmt.Errorf(
			"Conflicting block height between "+
				"notifications: onBlockConnected=%d, "+
				"onFilteredBlockConnected=%d.", curBlockHeight,
			curFilteredBlockHeight,
		)
	}
	return txCount[0], curBlockHeight, nil
}

// banPeer bans and disconnects the requested harness from the ChainService instance for BanDuration seconds.
func banPeer(svc *spv.ChainService, harness *rpctest.Harness) {
	peers := svc.Peers()
	for _, peer := range peers {
		if peer.Addr() == harness.P2PAddress() {
			svc.BanPeer(peer)
			peer.Disconnect()
		}
	}
}

// goroutineDump returns a string with the current goroutine dump in order to show what's going on in case of timeout.
func goroutineDump() string {
	buf := make([]byte, 1<<18)
	runtime.Stack(buf, true)
	return string(buf)
}
