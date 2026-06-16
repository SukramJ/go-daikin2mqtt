// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Command daikin2mqtt-util is the diagnostic / auth helper CLI. It drives
// the OAuth2 flow and inspects the ONECTA cloud (device dumps, datapoint
// listings, manual PATCH tests, rate-limit status, catalog checks).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/SukramJ/go-daikin2mqtt/internal/version"
)

const usage = `daikin2mqtt-util — diagnostic / auth helper

Usage:
  daikin2mqtt-util <command> [flags]

Commands:
  auth           Run the interactive OAuth2 flow and write the token store
  devices        Dump GET /v1/gateway-devices
  points <id>    List management points / characteristics of a device
  set ...        Send a test PATCH to a characteristic
  ratelimit      Show the last seen rate-limit budget
  catalog-check  Report live characteristics not covered by characteristics.yaml
  version        Print version and exit
`

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	var err error
	switch args[0] {
	case "version":
		fmt.Println(version.String())
	case "auth":
		err = runAuth(args[1:])
	case "devices":
		err = runDevices(args[1:])
	case "points":
		err = runPoints(args[1:])
	case "set":
		err = runSet(args[1:])
	case "ratelimit":
		err = runRateLimit(args[1:])
	case "catalog-check":
		err = runCatalogCheck(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
