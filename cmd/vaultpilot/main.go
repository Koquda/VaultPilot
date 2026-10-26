package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Koquda/VaultPilot/internal/store"
	"github.com/Koquda/VaultPilot/internal/web"
)

func main() {
	address := flag.String("addr", "127.0.0.1:8080", "loopback address to listen on")
	directory := flag.String("data", "data", "directory for local workspace state")
	flag.Parse()
	host, _, err := net.SplitHostPort(*address)
	if err != nil || host != "127.0.0.1" {
		log.Fatal("Use a loopback address such as 127.0.0.1:8080. Network exposure is not supported.")
	}
	st, err := store.Open(filepath.Join(*directory, "workspace.json"), time.Now().UTC())
	if err != nil {
		log.Fatalf("Could not open the workspace. Existing data has been preserved. %v", err)
	}
	defer st.Close()
	handler, err := web.New(st, *address)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stopped
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()
	fmt.Printf("VaultPilot is ready at http://%s\nYour workspace stays in %s\n", *address, *directory)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
