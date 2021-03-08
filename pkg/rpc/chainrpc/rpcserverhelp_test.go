package chainrpc

import (
	"testing"
)

// TestHelp ensures the help is reasonably accurate by checking that every command specified also has result types
// defined and the one-line usage and help text can be generated for them.
func TestHelp(t *testing.T) {
	// Ensure there are result types specified for every handler.
	for k := range RPCHandlers {
		if _, ok := ResultTypes[k]; !ok {
			t.Errorf("RPC handler defined for method '%v' without "+
				"also specifying result types", k)
			continue
		}
	}
	for k := range WSHandlers {
		if _, ok := ResultTypes[k]; !ok {
			t.Errorf("RPC handler defined for method '%v' without "+
				"also specifying result types", k)
			continue
		}
	}
	// Ensure the usage for every command can be generated without errors.
	helpCacher := NewHelpCacher()
	if _, e = helpCacher.RPCUsage(true); dbg.Chk(e) {
		t.Fatalf("Failed to generate one-line usage: %v", err)
	}
	if _, e = helpCacher.RPCUsage(true); dbg.Chk(e) {
		t.Fatalf("Failed to generate one-line usage (cached): %v", err)
	}
	// Ensure the help for every command can be generated without errors.
	for k := range RPCHandlers {
		if _, e = helpCacher.RPCMethodHelp(k); dbg.Chk(e) {
			t.Errorf("Failed to generate help for method '%v': %v",
				k, err)
			continue
		}
		if _, e = helpCacher.RPCMethodHelp(k); dbg.Chk(e) {
			t.Errorf("Failed to generate help for method '%v'"+
				"(cached): %v", k, err)
			continue
		}
	}
	for k := range WSHandlers {
		if _, e = helpCacher.RPCMethodHelp(k); dbg.Chk(e) {
			t.Errorf("Failed to generate help for method '%v': %v",
				k, err)
			continue
		}
		if _, e = helpCacher.RPCMethodHelp(k); dbg.Chk(e) {
			t.Errorf("Failed to generate help for method '%v'"+
				"(cached): %v", k, err)
			continue
		}
	}
}
