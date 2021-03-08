package main

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"os"
	
	ec "github.com/p9c/pod/pkg/coding/elliptic"
)

func main() {
	
	fi, e := os.Create("secp256k1.go")
	
	if e != nil  {
				ftl.Ln(err)
	}
	defer func() {
		if e := fi.Close(); dbg.Chk(e) {
		}
	}()
	
	// Compress the serialized byte points.
	serialized := ec.S256().SerializedBytePoints()
	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	
	if _, e = w.Write(serialized); dbg.Chk(e) {
				os.Exit(1)
	}
	if e := w.Close(); dbg.Chk(e) {
	}
	
	// Encode the compressed byte points with base64.
	encoded := make([]byte, base64.StdEncoding.EncodedLen(compressed.Len()))
	base64.StdEncoding.Encode(encoded, compressed.Bytes())
	_, _ = fmt.Fprintln(fi, "")
	_, _ = fmt.Fprintln(fi, "")
	_, _ = fmt.Fprintln(fi, "")
	_, _ = fmt.Fprintln(fi)
	_, _ = fmt.Fprintln(fi, "package ec")
	_, _ = fmt.Fprintln(fi)
	_, _ = fmt.Fprintln(fi, "// Auto-generated file (see genprecomps.go)")
	_, _ = fmt.Fprintln(fi, "// DO NOT EDIT")
	_, _ = fmt.Fprintln(fi)
	_, _ = fmt.Fprintf(fi, "var secp256k1BytePoints = %q\n", string(encoded))
	a1, b1, a2, b2 := ec.S256().EndomorphismVectors()
	_, _ = fmt.Fprintln(fi,
		"// The following values are the computed linearly "+
			"independent vectors needed to make use of the secp256k1 "+
			"endomorphism:")
	_, _ = fmt.Fprintf(fi, "// a1: %x\n", a1)
	_, _ = fmt.Fprintf(fi, "// b1: %x\n", b1)
	_, _ = fmt.Fprintf(fi, "// a2: %x\n", a2)
	_, _ = fmt.Fprintf(fi, "// b2: %x\n", b2)
}
