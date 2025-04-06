package main

import (
	"context"
	"flag"
	"github.com/rs/zerolog/log"
	"github.com/xdire/dtc-gateway/certsvc"
	"github.com/xdire/dtc-gateway/entities"
	"github.com/xdire/dtc-gateway/gatewaysvc"
	_ "google.golang.org/grpc/credentials/insecure"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// Parse command line flags
	publicHost := flag.String("public-host", "0.0.0.0", "Host to bind public interface")
	publicPort := flag.Int("public-port", 50051, "Port for public interface")
	controlHost := flag.String("control-host", "0.0.0.0", "Host to bind control interface")
	controlPort := flag.Int("control-port", 50052, "Port for control interface")
	tunnelPortBase := flag.Int("tunnel-port-base", 60000, "Base port for tunnel connections")
	tlsEnabled := flag.Bool("tls", false, "Enable TLS")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file")
	tlsKey := flag.String("tls-key", "", "TLS key file")
	authToken := flag.String("auth-token", "", "Auth token for agent registration")
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	httpPort := flag.Int("http-port", 8443, "Port for HTTP certificate server (0 to disable)")

	flag.Parse()

	// Validate required flags
	if *authToken == "" {
		log.Fatal().Msg("auth-token is required")
	}

	if *tlsEnabled && (*tlsCert == "" || *tlsKey == "") {
		log.Fatal().Msg("tls-cert and tls-key are required when TLS is enabled")
	}

	// Create config
	config := entities.Config{
		PublicHost:     *publicHost,
		PublicPort:     *publicPort,
		ControlHost:    *controlHost,
		ControlPort:    *controlPort,
		TunnelPortBase: *tunnelPortBase,
		TLSEnabled:     *tlsEnabled,
		TLSCertFile:    *tlsCert,
		TLSKeyFile:     *tlsKey,
		AuthToken:      *authToken,
		LogLevel:       *logLevel,
		HTTPPort:       *httpPort,
	}

	// Create server
	server := gatewaysvc.NewGatewayServer(config)

	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigCh
		cancel()
	}()

	if config.HTTPPort > 0 {
		server.Logger.Info().
			Int("port", config.HTTPPort).
			Msg("Starting certificate HTTP server")

		certServer := certsvc.NewCertificateServer(server, config.HTTPPort, server.Logger)
		if err := certServer.Start(ctx); err != nil {
			log.Fatal().Err(err).Msg("Failed to start server")
		}
	}

	// Start server
	if err := server.Start(ctx); err != nil {
		log.Fatal().Err(err).Msg("Failed to start server")
	}

}
