package btcjson_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/p9c/pod/pkg/btcjson"
)

// TestPodExtCmds tests all of the pod extended commands marshal and unmarshal into valid results include handling of
// optional fields being omitted in the marshalled command, while optional fields with defaults have the default
// assigned on unmarshalled commands.
func TestPodExtCmds(t *testing.T) {
	t.Parallel()
	testID := 1
	tests := []struct {
		name         string
		newCmd       func() (interface{}, error)
		staticCmd    func() interface{}
		marshalled   string
		unmarshalled interface{}
	}{
		{
			name: "debuglevel",
			newCmd: func() (interface{}, error) {
				return btcjson.NewCmd("debuglevel", "trace")
			},
			staticCmd: func() interface{} {
				return btcjson.NewDebugLevelCmd("trace")
			},
			marshalled: `{"jsonrpc":"1.0","method":"debuglevel","netparams":["trace"],"id":1}`,
			unmarshalled: &btcjson.DebugLevelCmd{
				LevelSpec: "trace",
			},
		},
		{
			name: "node",
			newCmd: func() (interface{}, error) {
				return btcjson.NewCmd("node", btcjson.NRemove, "1.1.1.1")
			},
			staticCmd: func() interface{} {
				return btcjson.NewNodeCmd("remove", "1.1.1.1", nil)
			},
			marshalled: `{"jsonrpc":"1.0","method":"node","netparams":["remove","1.1.1.1"],"id":1}`,
			unmarshalled: &btcjson.NodeCmd{
				SubCmd: btcjson.NRemove,
				Target: "1.1.1.1",
			},
		},
		{
			name: "node",
			newCmd: func() (interface{}, error) {
				return btcjson.NewCmd("node", btcjson.NDisconnect, "1.1.1.1")
			},
			staticCmd: func() interface{} {
				return btcjson.NewNodeCmd("disconnect", "1.1.1.1", nil)
			},
			marshalled: `{"jsonrpc":"1.0","method":"node","netparams":["disconnect","1.1.1.1"],"id":1}`,
			unmarshalled: &btcjson.NodeCmd{
				SubCmd: btcjson.NDisconnect,
				Target: "1.1.1.1",
			},
		},
		{
			name: "node",
			newCmd: func() (interface{}, error) {
				return btcjson.NewCmd("node", btcjson.NConnect, "1.1.1.1", "perm")
			},
			staticCmd: func() interface{} {
				return btcjson.NewNodeCmd("connect", "1.1.1.1", btcjson.String("perm"))
			},
			marshalled: `{"jsonrpc":"1.0","method":"node","netparams":["connect","1.1.1.1","perm"],"id":1}`,
			unmarshalled: &btcjson.NodeCmd{
				SubCmd:        btcjson.NConnect,
				Target:        "1.1.1.1",
				ConnectSubCmd: btcjson.String("perm"),
			},
		},
		{
			name: "node",
			newCmd: func() (interface{}, error) {
				return btcjson.NewCmd("node", btcjson.NConnect, "1.1.1.1", "temp")
			},
			staticCmd: func() interface{} {
				return btcjson.NewNodeCmd("connect", "1.1.1.1", btcjson.String("temp"))
			},
			marshalled: `{"jsonrpc":"1.0","method":"node","netparams":["connect","1.1.1.1","temp"],"id":1}`,
			unmarshalled: &btcjson.NodeCmd{
				SubCmd:        btcjson.NConnect,
				Target:        "1.1.1.1",
				ConnectSubCmd: btcjson.String("temp"),
			},
		},
		{
			name: "generate",
			newCmd: func() (interface{}, error) {
				return btcjson.NewCmd("generate", 1)
			},
			staticCmd: func() interface{} {
				return btcjson.NewGenerateCmd(1)
			},
			marshalled: `{"jsonrpc":"1.0","method":"generate","netparams":[1],"id":1}`,
			unmarshalled: &btcjson.GenerateCmd{
				NumBlocks: 1,
			},
		},
		{
			name: "getbestblock",
			newCmd: func() (interface{}, error) {
				return btcjson.NewCmd("getbestblock")
			},
			staticCmd: func() interface{} {
				return btcjson.NewGetBestBlockCmd()
			},
			marshalled:   `{"jsonrpc":"1.0","method":"getbestblock","netparams":[],"id":1}`,
			unmarshalled: &btcjson.GetBestBlockCmd{},
		},
		{
			name: "getcurrentnet",
			newCmd: func() (interface{}, error) {
				return btcjson.NewCmd("getcurrentnet")
			},
			staticCmd: func() interface{} {
				return btcjson.NewGetCurrentNetCmd()
			},
			marshalled:   `{"jsonrpc":"1.0","method":"getcurrentnet","netparams":[],"id":1}`,
			unmarshalled: &btcjson.GetCurrentNetCmd{},
		},
		{
			name: "getheaders",
			newCmd: func() (interface{}, error) {
				return btcjson.NewCmd("getheaders", []string{}, "")
			},
			staticCmd: func() interface{} {
				return btcjson.NewGetHeadersCmd(
					[]string{},
					"",
				)
			},
			marshalled: `{"jsonrpc":"1.0","method":"getheaders","netparams":[[],""],"id":1}`,
			unmarshalled: &btcjson.GetHeadersCmd{
				BlockLocators: []string{},
				HashStop:      "",
			},
		},
		{
			name: "getheaders - with arguments",
			newCmd: func() (interface{}, error) {
				return btcjson.NewCmd("getheaders", []string{
					"000000000000000001f1739002418e2f9a84c47a4fd2a0eb7a787a6b7dc12f16",
					"0000000000000000026f4b7f56eef057b32167eb5ad9ff62006f1807b7336d10",
				}, "000000000000000000ba33b33e1fad70b69e234fc24414dd47113bff38f523f7")
			},
			staticCmd: func() interface{} {
				return btcjson.NewGetHeadersCmd(
					[]string{
						"000000000000000001f1739002418e2f9a84c47a4fd2a0eb7a787a6b7dc12f16",
						"0000000000000000026f4b7f56eef057b32167eb5ad9ff62006f1807b7336d10",
					},
					"000000000000000000ba33b33e1fad70b69e234fc24414dd47113bff38f523f7",
				)
			},
			marshalled: `{"jsonrpc":"1.0","method":"getheaders",
				"netparams":[["000000000000000001f1739002418e2f9a84c47a4fd2a0eb7a787a6b7dc12f16","0000000000000000026f4b7f56eef057b32167eb5ad9ff62006f1807b7336d10"],"000000000000000000ba33b33e1fad70b69e234fc24414dd47113bff38f523f7"],"id":1}`,
			unmarshalled: &btcjson.GetHeadersCmd{
				BlockLocators: []string{
					"000000000000000001f1739002418e2f9a84c47a4fd2a0eb7a787a6b7dc12f16",
					"0000000000000000026f4b7f56eef057b32167eb5ad9ff62006f1807b7336d10",
				},
				HashStop: "000000000000000000ba33b33e1fad70b69e234fc24414dd47113bff38f523f7",
			},
		},
		{
			name: "version",
			newCmd: func() (interface{}, error) {
				return btcjson.NewCmd("version")
			},
			staticCmd: func() interface{} {
				return btcjson.NewVersionCmd()
			},
			marshalled:   `{"jsonrpc":"1.0","method":"version","netparams":[],"id":1}`,
			unmarshalled: &btcjson.VersionCmd{},
		},
	}
	t.Logf("Running %d tests", len(tests))
	for i, test := range tests {
		// Marshal the command as created by the new static command creation function.
		marshalled, e := btcjson.MarshalCmd(testID, test.staticCmd())
		if e != nil  {
			t.Errorf("MarshalCmd #%d (%s) unexpected error: %v", i,
				test.name, e)
			continue
		}
		if !bytes.Equal(marshalled, []byte(test.marshalled)) {
			t.Errorf("Test #%d (%s) unexpected marshalled data - "+
				"got %s, want %s", i, test.name, marshalled,
				test.marshalled)
			continue
		}
		// Ensure the command is created without error via the generic new command creation function.
		cmd, e := test.newCmd()
		if e != nil  {
			t.Errorf("Test #%d (%s) unexpected NewCmd error: %v ",
				i, test.name, e)
		}
		// Marshal the command as created by the generic new command creation function.
		marshalled, e = btcjson.MarshalCmd(testID, cmd)
		if e != nil  {
			t.Errorf("MarshalCmd #%d (%s) unexpected error: %v", i,
				test.name, e)
			continue
		}
		if !bytes.Equal(marshalled, []byte(test.marshalled)) {
			t.Errorf("Test #%d (%s) unexpected marshalled data - "+
				"got %s, want %s", i, test.name, marshalled,
				test.marshalled)
			continue
		}
		var request btcjson.Request
		if e := json.Unmarshal(marshalled, &request); E.Chk(e) {
			t.Errorf("Test #%d (%s) unexpected error while "+
				"unmarshalling JSON-RPC request: %v", i,
				test.name, e)
			continue
		}
		cmd, e = btcjson.UnmarshalCmd(&request)
		if e != nil  {
			t.Errorf("UnmarshalCmd #%d (%s) unexpected error: %v", i,
				test.name, e)
			continue
		}
		if !reflect.DeepEqual(cmd, test.unmarshalled) {
			t.Errorf("Test #%d (%s) unexpected unmarshalled command "+
				"- got %s, want %s", i, test.name,
				fmt.Sprintf("(%T) %+[1]v", cmd),
				fmt.Sprintf("(%T) %+[1]v\n", test.unmarshalled))
			continue
		}
	}
}
