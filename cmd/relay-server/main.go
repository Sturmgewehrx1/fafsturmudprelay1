package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"fafsturmudprelay/internal/relay"
)

func main() {
	udpAddr := flag.String("udp-addr", ":10000", "UDP listen address for relay packets")
	tcpAddr := flag.String("tcp-addr", ":443", "TCP fallback listen address (empty string = disabled; port 443 requires admin/root)")
	httpAddr := flag.String("http-addr", "0.0.0.0:8080", "HTTP/HTTPS listen address for API")
	apiKey := flag.String("api-key", "mussfuerproduktiongeandertwerdenxyz45k8", "API key for HTTP endpoints (mutating requests require Authorization: Bearer <key>)")
	tlsCert := flag.String("tls-cert", "", "Path to TLS certificate PEM file (enables HTTPS when set)")
	tlsKey := flag.String("tls-key", "", "Path to TLS private key PEM file (required when -tls-cert is set)")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	srv := relay.NewServer(relay.ServerConfig{
		UDPAddr:  *udpAddr,
		TCPAddr:  *tcpAddr,
		HTTPAddr: *httpAddr,
		APIKey:   *apiKey,
		TLSCert:  *tlsCert,
		TLSKey:   *tlsKey,
	})

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("relay server: %v", err)
	}
}
