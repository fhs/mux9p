// This program announces and multiplexes a 9P service.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/fhs/mux9p"
)

var (
	noauth  = flag.Bool("n", false, "no authentication; respond to Tauth messages with an error")
	verbose = flag.Int("v", 0, "verbosity")
	logging = flag.Bool("l", false, "logging; write a debugging log to addr.log")
)

func usage() {
	fmt.Fprintf(os.Stderr, "usage: 9pserve [flags] address\n")
	fmt.Fprintf(os.Stderr, "\treads/writes 9P messages on stdin/stdout\n")
	fmt.Fprintf(os.Stderr, "\n")
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	flag.Parse()
	if flag.NArg() != 1 {
		usage()
	}
	addr := flag.Arg(0)

	network, address := parseAddr(addr)

	f, err := os.Create(fmt.Sprintf("%s.log", address))
	if err != nil {
		log.Fatalf("create failed: %v", err)
	}
	defer f.Close()

	mux9p.Listen(network, address, stdio{}, &mux9p.Config{
		NoAuth: *noauth,
		Logger: log.New(f, "", log.LstdFlags),
	})
}

func parseAddr(dial string) (net, addr string) {
	if dial == "" {
		panic("empty dial string")
	}
	f := strings.SplitN(dial, "!", 3)
	if f[0] == "net" {
		panic("unsupported network net")
	}
	return f[0], strings.Join(f[1:], ":")
}

type stdio struct{}

func (s stdio) Read(b []byte) (int, error)  { return s.Read(b) }
func (s stdio) Write(b []byte) (int, error) { return s.Write(b) }
