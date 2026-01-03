package main

import (
	"flag"
	"fmt"
	"os"

	"srv.exe.dev/srv"
)

var flagListenAddr = flag.String("listen", ":8000", "address to listen on")

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func run() error {
	flag.Parse()
	hostname := os.Getenv("HOSTNAME")
	if hostname == "" {
		hostname = "localhost:8000"
	}
	server, err := srv.New(hostname)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	return server.Serve(*flagListenAddr)
}
