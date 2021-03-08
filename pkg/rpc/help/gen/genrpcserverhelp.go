package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/p9c/pod/pkg/rpc/btcjson"
	rpchelp "github.com/p9c/pod/pkg/rpc/help"
	// rpchelp "github.com/p9c/pod/pkg/rpc/help"
)

var outputFile = func() *os.File {
	fi, e := os.Create("../rpcserverhelp.go")
	if e != nil  {
				ftl.Ln(err)
	}
	return fi
}()

func writefln(format string, args ...interface{}) {
	_, e := fmt.Fprintf(outputFile, format, args...)
	if e != nil  {
				ftl.Ln(err)
	}
	_, e = outputFile.Write([]byte{'\n'})
	if e != nil  {
				ftl.Ln(err)
	}
}
func writeLocaleHelp(locale, goLocale string, descs map[string]string) {
	funcName := "helpDescs" + goLocale
	writefln("func %s() map[string]string {", funcName)
	writefln("return map[string]string{")
	for i := range rpchelp.Methods {
		m := &rpchelp.Methods[i]
		helpText, e := btcjson.GenerateHelp(m.Method, descs, m.ResultTypes...)
		if e != nil  {
						ftl.Ln(err)
		}
		writefln("%q: %q,", m.Method, helpText)
	}
	writefln("}")
	writefln("}")
}
func writeLocales() {
	writefln("var localeHelpDescs = map[string]func() map[string]string{")
	for _, h := range rpchelp.HelpDescs {
		writefln("%q: helpDescs%s,", h.Locale, h.GoLocale)
	}
	writefln("}")
}
func writeUsage() {
	usageStrs := make([]string, len(rpchelp.Methods))
	var e error
	for i := range rpchelp.Methods {
		usageStrs[i], e = btcjson.MethodUsageText(rpchelp.Methods[i].Method)
		if e != nil  {
						ftl.Ln(err)
		}
	}
	usages := strings.Join(usageStrs, "\n")
	writefln("var requestUsages = %q", usages)
}
func main() {
	defer func() {
		if e := outputFile.Close(); dbg.Chk(e) {
		}
	}()
	packageName := "main"
	if len(os.Args) > 1 {
		packageName = os.Args[1]
	}
	writefln("// AUTOGENERATED by internal/rpchelp/genrpcserverhelp.go; do not edit.")
	writefln("")
	writefln("package %s", packageName)
	writefln("")
	for _, h := range rpchelp.HelpDescs {
		writeLocaleHelp(h.Locale, h.GoLocale, h.Descs)
		writefln("")
	}
	writeLocales()
	writefln("")
	writeUsage()
}
