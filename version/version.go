package version

import "fmt"

var (

	// URL is the git URL for the repository
	URL = "github.com/p9c/pod"
	// GitRef is the gitref, as in refs/heads/branchname
	GitRef = "refs/heads/l0k1"
	// GitCommit is the commit hash of the current HEAD
	GitCommit = "6fb5f05c37951bab864fe79b2a3b263dfb906594"
	// BuildTime stores the time when the current binary was built
	BuildTime = "2021-03-11T21:16:02+01:00"
	// Tag lists the Tag on the build, adding a + to the newest Tag if the commit is
	// not that commit
	Tag = "v1.9.16+"
	// PathBase is the path base returned from runtime caller
	PathBase = "/home/loki/src/github.com/p9c/pod/"
)

// Get returns a pretty printed version information string
func Get() string {
	return fmt.Sprint(
		"ParallelCoin Pod\n"+
		"	git repository: "+URL+"\n",
		"	branch: "+GitRef+"\n"+
		"	commit: "+GitCommit+"\n"+
		"	built: "+BuildTime+"\n"+
		"	Tag: "+Tag+"\n",
	)
}
