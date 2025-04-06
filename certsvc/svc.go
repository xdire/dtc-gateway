package certsvc

import (
	"context"
	"fmt"
	"github.com/xdire/dtc-gateway/gatewaysvc"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/xdire/dtc-proto/gol/gateway"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// CertificateCache stores CA certificates
type CertificateCache struct {
	mu           sync.RWMutex
	certificates map[string]*gateway.Certificate
}

// Get gets a certificate from the cache
func (c *CertificateCache) Get(certType string) *gateway.Certificate {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cert, exists := c.certificates[certType]
	if !exists {
		return nil
	}

	// Check if certificate is expired (1 hour cache time)
	if time.Since(cert.Timestamp.AsTime()) > time.Hour {
		return nil
	}

	return cert
}

// Set sets a certificate in the cache
func (c *CertificateCache) Set(certType string, cert *gateway.Certificate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.certificates[certType] = cert
}

// NewCertificateCache creates a new certificate cache
func NewCertificateCache() *CertificateCache {
	return &CertificateCache{
		certificates: make(map[string]*gateway.Certificate),
	}
}

// CertificateServer handles HTTP requests for certificates
type CertificateServer struct {
	server    *http.Server
	gateway   *gatewaysvc.GatewayServer
	certCache *CertificateCache
	httpPort  int
	logger    zerolog.Logger
}

// NewCertificateServer creates a new certificate server
func NewCertificateServer(gateway *gatewaysvc.GatewayServer, httpPort int, logger zerolog.Logger) *CertificateServer {
	return &CertificateServer{
		gateway:   gateway,
		certCache: NewCertificateCache(),
		httpPort:  httpPort,
		logger:    logger.With().Str("component", "cert-server").Logger(),
	}
}

// Start starts the certificate server
func (cs *CertificateServer) Start(ctx context.Context) error {
	// Create a new HTTP server
	mux := http.NewServeMux()

	// Register handlers
	mux.HandleFunc("/security/ca", cs.handleCACert)

	// Configure the server
	cs.server = &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", cs.httpPort),
		Handler: mux,
	}

	// Start the server in a goroutine
	go func() {
		cs.logger.Info().Int("port", cs.httpPort).Msg("Starting certificate server")
		if err := cs.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			cs.logger.Error().Err(err).Msg("Certificate server error")
		}
	}()

	// Wait for context cancellation
	go func() {
		<-ctx.Done()
		cs.Shutdown()
	}()

	return nil
}

// Shutdown shuts down the certificate server
func (cs *CertificateServer) Shutdown() {
	if cs.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		cs.logger.Info().Msg("Shutting down certificate server")
		if err := cs.server.Shutdown(ctx); err != nil {
			cs.logger.Error().Err(err).Msg("Certificate server shutdown error")
		}
	}
}

// handleCACert handles CA certificate requests
func (cs *CertificateServer) handleCACert(w http.ResponseWriter, r *http.Request) {
	cs.logger.Info().Str("remote", r.RemoteAddr).Msg("Received CA certificate request")

	// Try to get certificate from cache
	cert := cs.certCache.Get("ca")

	// If not in cache, request from an agent
	if cert == nil {
		var err error
		cert, err = cs.requestCertificateFromAgent(r.Context(), "ca")
		if err != nil {
			cs.logger.Error().Err(err).Msg("Failed to get CA certificate from agent")
			http.Error(w, "CA certificate not available", http.StatusInternalServerError)
			return
		}

		// Store in cache
		cs.certCache.Set("ca", cert)
	}

	// Set content type
	contentType := cert.ContentType
	if contentType == "" {
		contentType = "application/x-pem-file"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "attachment; filename="+cert.Filename)

	// Write the certificate data
	_, err := w.Write(cert.Data)
	if err != nil {
		cs.logger.Error().Err(err).Msg("Error writing CA certificate")
	}
}

// requestCertificateFromAgent requests a certificate from an agent
func (cs *CertificateServer) requestCertificateFromAgent(ctx context.Context, certType string) (*gateway.Certificate, error) {
	// Get an agent that can provide the certificate
	agent := cs.gateway.SelectAgent()
	if agent == nil {
		return nil, fmt.Errorf("no agent available")
	}

	// Create certificate request
	certReq := &gateway.ControlMessage{
		Type:      gateway.ControlMessage_CERTIFICATE_REQUEST,
		AgentId:   agent.ID,
		Timestamp: timestamppb.Now(),
		Payload: &gateway.ControlMessage_CertRequest{
			CertRequest: &gateway.CertificateRequest{
				CertType: certType,
			},
		},
	}

	// Create a response channel
	respChan := make(chan *gateway.Certificate, 1)
	errChan := make(chan error, 1)

	// Store response channel in agent
	agent.Mutex.Lock()
	if agent.CertRespChannels == nil {
		agent.CertRespChannels = make(map[string]chan<- *gateway.Certificate)
	}
	if agent.CertErrChannels == nil {
		agent.CertErrChannels = make(map[string]chan<- error)
	}

	// Use timestamp as request ID
	requestID := certReq.Timestamp.AsTime().Format(time.RFC3339Nano)
	agent.CertRespChannels[requestID] = respChan
	agent.CertErrChannels[requestID] = errChan
	agent.Mutex.Unlock()

	// Clean up channels when done
	defer func() {
		agent.Mutex.Lock()
		delete(agent.CertRespChannels, requestID)
		delete(agent.CertErrChannels, requestID)
		agent.Mutex.Unlock()
	}()

	// Send request
	agent.Mutex.Lock()
	if agent.ControlStream == nil {
		agent.Mutex.Unlock()
		return nil, fmt.Errorf("agent has no control stream")
	}

	err := agent.ControlStream.Send(certReq)
	agent.Mutex.Unlock()
	if err != nil {
		return nil, fmt.Errorf("failed to send certificate request: %w", err)
	}

	// Wait for response with timeout
	select {
	case cert := <-respChan:
		return cert, nil
	case err := <-errChan:
		return nil, err
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("certificate request timed out")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
