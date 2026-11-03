// This program is only built by isolated tests. It performs no network requests.
package main

import (
	"flag"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

func main() {
	input := flag.String("f", "", "")
	output := flag.String("o", "", "")
	record := flag.String("fixture-record", "", "")
	_ = flag.Bool("dd", false, "")
	flag.Parse()
	data, err := os.ReadFile(*input)
	if err != nil {
		panic(err)
	}
	first := strings.TrimSpace(strings.Split(string(data), "\n")[0])
	prefix, err := netip.ParsePrefix(first)
	if err != nil {
		panic(err)
	}
	if *record != "" {
		file, err := os.OpenFile(*record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			panic(err)
		}
		fmt.Fprintln(file, "executed")
		file.Close()
	}
	if err := os.WriteFile(*output, []byte("IP 地址,延迟\n"+prefix.Addr().Next().String()+",1\n"), 0600); err != nil {
		panic(err)
	}
}
