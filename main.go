// Command probe-jmap serves a Probe action that calls JMAP methods. Probe
// starts it for a step that uses github.com/mozership/probe-jmap@<commit>.
package main

import (
	"github.com/hashicorp/go-hclog"
	"github.com/mozership/probe/sdk"
)

// version is set at build time.
var version = "dev"

func main() {
	sdk.Serve(func(log hclog.Logger) sdk.Action {
		return &Action{log: log}
	})
}
