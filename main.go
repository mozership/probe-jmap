// Command probe-jmap serves a Probe action that calls JMAP methods. Probe
// starts it for a step that uses github.com/mozership/probe-jmap@<commit>.
package main

import (
	"github.com/hashicorp/go-hclog"
	"github.com/linyows/probe/actionrpc"
)

// version is set at build time.
var version = "dev"

func main() {
	actionrpc.Serve(func(log hclog.Logger) actionrpc.Action {
		return &Action{log: log}
	})
}
