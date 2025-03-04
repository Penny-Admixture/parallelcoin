package app

import (
	"github.com/gookit/color"
	"github.com/p9c/pod/pkg/logg"
	"github.com/p9c/pod/pkg/pod"
	"os"
	"os/exec"
	
	"github.com/p9c/pod/pkg/podconfig"
	
	"github.com/urfave/cli"
)

var initHandle = func(cx *pod.State) func(c *cli.Context) (e error) {
	return func(c *cli.Context) (e error) {
		logg.AppColorizer = color.Bit24(255, 255, 255, false).Sprint
		logg.App = "  init"
		I.Ln("running configuration and wallet initialiser")
		podconfig.Configure(cx, "init", true)
		args := append(os.Args[1:len(os.Args)-1], "wallet")
		D.Ln(args)
		var command []string
		command = append(command, os.Args[0])
		command = append(command, args...)
		// command = apputil.PrependForWindows(command)
		firstWallet := exec.Command(command[0], command[1:]...)
		firstWallet.Stdin = os.Stdin
		firstWallet.Stdout = os.Stdout
		firstWallet.Stderr = os.Stderr
		e = firstWallet.Run()
		D.Ln("running it a second time for mining addresses")
		secondWallet := exec.Command(command[0], command[1:]...)
		secondWallet.Stdin = os.Stdin
		secondWallet.Stdout = os.Stdout
		secondWallet.Stderr = os.Stderr
		e = firstWallet.Run()
		I.Ln(
			"you should be ready to go to sync and mine on the network:", cx.ActiveNet.Name,
			"using datadir:", *cx.Config.DataDir,
		)
		return e
	}
}
