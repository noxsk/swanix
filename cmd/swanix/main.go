// Command swanix is the OpenWrt multi-WAN controller built on sing-box.
// Two entry points share this CLI: entry_lite.go (tag mw_lite, no proxy code)
// and entry_full.go (full sing-box runtime plus the multiwan service).
package main

import "os"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
