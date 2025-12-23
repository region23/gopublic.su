package main

import (
	"gopublic/internal/client/cli"
)

// ServerAddr is set via ldflags during build. e.g. -X main.ServerAddr=example.com:4443
var ServerAddr = "localhost:4443"

func main() {
	cli.Init(ServerAddr)
	cli.Execute()
}
