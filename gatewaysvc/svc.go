package gatewaysvc

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/xdire/dtc-gateway/entities"
	"github.com/xdire/dtc-proto/gol/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// GatewayServer implements the gateway service
type GatewayServer struct {
	gateway.UnimplementedGatewayServiceServer

	config   entities.Config
	agents   map[string]*entities.Agent
	sessions map[string]*entities.ClientSession
	mu       sync.RWMutex

	// Stats
	stats struct {
		TotalAgents        int32
		ActiveAgents       int32
		TotalSessions      int32
		ActiveSessions     int32
		TotalBytesSent     int64
		TotalBytesReceived int64
		CPUUsage           float32
		MemoryUsage        float32
		mu                 sync.Mutex
	}

	// Public-facing listener
	publicListener net.Listener

	// Logger
	Logger zerolog.Logger
}

// NewGatewayServer creates a new gateway server
func NewGatewayServer(config entities.Config) *GatewayServer {
	// Initialize Logger
	logLevel, err := zerolog.ParseLevel(config.LogLevel)
	if err != nil {
		logLevel = zerolog.InfoLevel
	}

	//Logger := zerolog.New(os.Stdout).
	//	Level(logLevel).
	//	With().
	//	Timestamp().
	//	Logger()
	logger := log.Level(logLevel).Output(zerolog.ConsoleWriter{Out: os.Stdout})

	return &GatewayServer{
		config:   config,
		agents:   make(map[string]*entities.Agent),
		sessions: make(map[string]*entities.ClientSession),
		Logger:   logger,
	}
}

// Start starts the gateway server
func (s *GatewayServer) Start(ctx context.Context) error {
	// Start the public listener
	publicAddr := fmt.Sprintf("%s:%d", s.config.PublicHost, s.config.PublicPort)
	var err error

	s.Logger.Info().
		Str("addr", publicAddr).
		Msg("Starting public listener")

	s.publicListener, err = net.Listen("tcp", publicAddr)
	if err != nil {
		return fmt.Errorf("failed to start public listener: %w", err)
	}

	// Start accepting public connections
	go s.acceptPublicConnections(ctx)

	// Start the control service
	controlAddr := fmt.Sprintf("%s:%d", s.config.ControlHost, s.config.ControlPort)

	s.Logger.Info().
		Str("addr", controlAddr).
		Msg("Starting control service")

	// Create gRPC server for control service
	var opts []grpc.ServerOption

	if s.config.TLSEnabled {
		// Load TLS credentials
		creds, err := credentials.NewServerTLSFromFile(
			s.config.TLSCertFile,
			s.config.TLSKeyFile,
		)
		if err != nil {
			return fmt.Errorf("failed to load TLS credentials: %w", err)
		}

		opts = append(opts, grpc.Creds(creds))
		s.Logger.Info().Msg("TLS enabled for control service")
	} else {
		s.Logger.Warn().Msg("TLS is disabled, control service is insecure")
	}

	grpcServer := grpc.NewServer(opts...)
	gateway.RegisterGatewayServiceServer(grpcServer, s)

	lis, err := net.Listen("tcp", controlAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on control address: %w", err)
	}

	// Start gRPC server
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			s.Logger.Error().
				Err(err).
				Msg("Control service failed")
		}
	}()

	// Start stats collector
	go s.collectStats(ctx)

	// Wait for context cancellation
	<-ctx.Done()

	// Shutdown
	s.Logger.Info().Msg("Shutting down gateway server")

	// Close the public listener
	if s.publicListener != nil {
		s.publicListener.Close()
	}

	// Close all agent connections
	s.mu.Lock()
	for _, agent := range s.agents {
		agent.Mutex.Lock()
		if agent.TunnelListener != nil {
			agent.TunnelListener.Close()
		}

		for _, tunnel := range agent.Tunnels {
			tunnel.Close()
		}
		agent.Mutex.Unlock()
	}
	s.mu.Unlock()

	// Stop the gRPC server
	grpcServer.GracefulStop()

	return nil
}

// acceptPublicConnections accepts connections from clients
func (s *GatewayServer) acceptPublicConnections(ctx context.Context) {
	for {
		conn, err := s.publicListener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				s.Logger.Error().
					Err(err).
					Msg("Error accepting public connection")

				time.Sleep(time.Second)
				continue
			}
		}

		go s.handleClientConnection(ctx, conn)
	}
}

// handleClientConnection handles a new client connection
func (s *GatewayServer) handleClientConnection(ctx context.Context, conn net.Conn) {
	// Select an agent
	agent := s.SelectAgent()
	if agent == nil {
		s.Logger.Error().Msg("No agent available")
		conn.Close()
		return
	}

	// Generate session ID and tunnel ID
	sessionID := uuid.New().String()
	tunnelID := uuid.New().String()
	clientID := fmt.Sprintf("client-%s", uuid.New().String()[:8])

	// Create session
	session := &entities.ClientSession{
		ID:           sessionID,
		ClientConn:   conn,
		AgentID:      agent.ID,
		ClientID:     clientID,
		TunnelID:     tunnelID,
		ConnectedAt:  time.Now(),
		LastActivity: time.Now(),
		Status:       gateway.ClientSessionStatus_ACTIVE,
	}

	s.mu.Lock()
	s.sessions[sessionID] = session
	s.stats.TotalSessions++
	s.stats.ActiveSessions++
	s.mu.Unlock()

	s.Logger.Info().
		Str("session_id", sessionID).
		Str("agent_id", agent.ID).
		Str("client_id", clientID).
		Msg("New client connected")

	// Notify agent about new client
	clientMsg := &gateway.ControlMessage{
		Type:      gateway.ControlMessage_CLIENT_CONNECTED,
		AgentId:   agent.ID,
		Timestamp: timestamppb.Now(),
		Payload: &gateway.ControlMessage_ClientSession{
			ClientSession: &gateway.ClientSession{
				Id:           sessionID,
				TunnelId:     tunnelID,
				ClientId:     clientID,
				AgentId:      agent.ID,
				ConnectedAt:  timestamppb.New(session.ConnectedAt),
				LastActivity: timestamppb.New(session.LastActivity),
				Status:       gateway.ClientSessionStatus_ACTIVE,
			},
		},
	}

	agent.Mutex.Lock()
	if agent.ControlStream != nil {
		err := agent.ControlStream.Send(clientMsg)
		if err != nil {
			agent.Mutex.Unlock()
			s.Logger.Error().
				Err(err).
				Str("session_id", sessionID).
				Str("agent_id", agent.ID).
				Msg("Failed to notify agent about new client")

			conn.Close()

			s.mu.Lock()
			delete(s.sessions, sessionID)
			s.stats.ActiveSessions--
			s.mu.Unlock()

			return
		}
	} else {
		agent.Mutex.Unlock()
		s.Logger.Error().
			Str("session_id", sessionID).
			Str("agent_id", agent.ID).
			Msg("Agent has no control stream")

		conn.Close()

		s.mu.Lock()
		delete(s.sessions, sessionID)
		s.stats.ActiveSessions--
		s.mu.Unlock()

		return
	}
	agent.Mutex.Unlock()

	// Wait for tunnel connection
	tunnelConn := s.waitForTunnelConnection(agent, tunnelID, 10*time.Second)
	if tunnelConn == nil {
		s.Logger.Error().
			Str("session_id", sessionID).
			Str("agent_id", agent.ID).
			Str("tunnel_id", tunnelID).
			Msg("Failed to establish tunnel connection")

		conn.Close()

		s.mu.Lock()
		delete(s.sessions, sessionID)
		s.stats.ActiveSessions--
		s.mu.Unlock()

		return
	}

	session.Mutex.Lock()
	session.TunnelConn = tunnelConn
	session.Mutex.Unlock()

	s.Logger.Info().
		Str("session_id", sessionID).
		Str("agent_id", agent.ID).
		Str("tunnel_id", tunnelID).
		Msg("Tunnel connection established")

	// Start proxying
	go s.proxyTraffic(ctx, session, false) // client to agent
	go s.proxyTraffic(ctx, session, true)  // agent to client

	// Configure TCP keep-alives on both connections
	if tcpConn, ok := session.ClientConn.(*net.TCPConn); ok {
		err := tcpConn.SetKeepAlive(true)
		if err != nil {
			log.Error().Err(err).Msg("Failed to set TCP keep alive, session connection can get unstable")
		}
		err = tcpConn.SetKeepAlivePeriod(15 * time.Second)
		if err != nil {
			log.Error().Err(err).Msg("Failed to set TCP keep alive, session connection can get unstable")
		}
	}
	if tcpConn, ok := session.TunnelConn.(*net.TCPConn); ok {
		err := tcpConn.SetKeepAlive(true)
		if err != nil {
			log.Error().Err(err).Msg("Failed to set TCP keep alive, tunnel connection for session can get unstable")
		}
		err = tcpConn.SetKeepAlivePeriod(15 * time.Second)
		if err != nil {
			log.Error().Err(err).Msg("Failed to set TCP keep alive, tunnel connection for session  can get unstable")
		}
	}
}

// proxyTraffic proxies traffic between connections
func (s *GatewayServer) proxyTraffic(ctx context.Context, session *entities.ClientSession, reversed bool) {
	var src, dst net.Conn

	session.Mutex.Lock()
	if reversed {
		src = session.TunnelConn
		dst = session.ClientConn
	} else {
		src = session.ClientConn
		dst = session.TunnelConn
	}
	direction := "client→server"
	if reversed {
		direction = "server→client"
	}
	session.Mutex.Unlock()

	buffer := make([]byte, 8192)

	// Create a ticker to periodically check connection health
	healthCheckTicker := time.NewTicker(30 * time.Second)
	defer healthCheckTicker.Stop()

	// Track activity times
	lastActivity := time.Now()
	absTimeout := 2 * time.Hour // Absolute maximum idle time (configurable)

	for {
		// Set a relatively short read deadline for each read operation
		err := src.SetReadDeadline(time.Now().Add(10 * time.Second))
		if err != nil {
			s.Logger.Debug().
				Err(err).
				Str("session_id", session.ID).
				Str("direction", direction).
				Msg("Failed to set read deadline")
		}

		select {
		case <-ctx.Done():
			s.Logger.Debug().
				Str("session_id", session.ID).
				Str("direction", direction).
				Msg("Connection closed by context")
			return
			return

		case <-healthCheckTicker.C:
			// Check if the connection has been completely idle for too long
			if time.Since(lastActivity) > absTimeout {
				s.Logger.Info().
					Str("session_id", session.ID).
					Str("direction", direction).
					Dur("idle_time", time.Since(lastActivity)).
					Msg("Closing connection due to extended inactivity")
				s.closeSession(session)
				return
			}

			// Verify the connection is still viable by pinging the peer
			// Use a brief deadline for these checks
			err := src.SetDeadline(time.Now().Add(1 * time.Second))
			if err != nil {
				s.Logger.Debug().
					Err(err).
					Str("session_id", session.ID).
					Str("direction", direction).
					Msg("Failed to set timeout for health check")
			}

			// For TCP connections, we can use the health check capability
			if tcpConn, ok := src.(*net.TCPConn); ok {
				// This is a non-blocking operation that just ensures the connection
				// can still send data and isn't in an error state
				if _, err := tcpConn.Write([]byte{}); err != nil {
					s.Logger.Info().
						Err(err).
						Str("session_id", session.ID).
						Str("direction", direction).
						Msg("Connection health check failed")
					s.closeSession(session)
					return
				}
			}

			// Reset deadline to unlimited for normal operations
			err = src.SetDeadline(time.Time{})
			if err != nil {
				s.Logger.Debug().
					Err(err).
					Str("session_id", session.ID).
					Str("direction", direction).
					Msg("Failed to reset deadline after health check")
			}

		default:

			// Try to read data with a short timeout
			n, err := src.Read(buffer)

			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// Timeout error, no data was on stream
					// Reset the deadline and continue
					src.SetDeadline(time.Time{})
					continue
				} else if err != io.EOF {
					s.Logger.Debug().
						Err(err).
						Str("session_id", session.ID).
						Str("direction", direction).
						Msg("Error reading from connection")
					s.closeSession(session)
					return
				} else {
					// EOF means connection closed normally
					s.Logger.Debug().
						Str("session_id", session.ID).
						Str("direction", direction).
						Msg("Connection closed (EOF)")
					s.closeSession(session)
					return
				}
			}

			if n > 0 {
				// We got some data, update activity time
				lastActivity = time.Now()

				// Reset read deadline
				err = src.SetDeadline(time.Time{})
				if err != nil {
					s.Logger.Debug().
						Err(err).
						Str("session_id", session.ID).
						Str("direction", direction).
						Msg("Failed to reset read deadline")
				}

				// Write the data to the destination
				_, err := dst.Write(buffer[:n])
				if err != nil {
					s.Logger.Debug().
						Err(err).
						Str("session_id", session.ID).
						Str("direction", direction).
						Msg("Error writing to connection")
					s.closeSession(session)
					return
				}

				// Update session stats
				session.Mutex.Lock()
				session.LastActivity = lastActivity
				if reversed {
					session.BytesReceived += int64(n)
				} else {
					session.BytesSent += int64(n)
				}
				session.Mutex.Unlock()

				// Update global stats
				s.stats.mu.Lock()
				if reversed {
					s.stats.TotalBytesReceived += int64(n)
				} else {
					s.stats.TotalBytesSent += int64(n)
				}
				s.stats.mu.Unlock()
			}
		}
	}
}

// closeSession closes a session
func (s *GatewayServer) closeSession(session *entities.ClientSession) {
	session.Mutex.Lock()
	if session.Status == gateway.ClientSessionStatus_CLOSED {
		session.Mutex.Unlock()
		return
	}

	// Mark session as closed
	session.Status = gateway.ClientSessionStatus_CLOSED

	// Get connections and agent ID before unlocking
	clientConn := session.ClientConn
	tunnelConn := session.TunnelConn
	agentID := session.AgentID
	sessionID := session.ID

	session.Mutex.Unlock()

	// Close connections
	if clientConn != nil {
		clientConn.Close()
	}

	if tunnelConn != nil {
		tunnelConn.Close()
	}

	// Get agent
	s.mu.Lock()
	agent, exists := s.agents[agentID]
	if exists {
		// Notify agent about client disconnection
		agent.Mutex.Lock()
		if agent.ControlStream != nil {
			disconnectMsg := &gateway.ControlMessage{
				Type:      gateway.ControlMessage_CLIENT_DISCONNECTED,
				AgentId:   agent.ID,
				Timestamp: timestamppb.Now(),
				Payload: &gateway.ControlMessage_ClientSession{
					ClientSession: &gateway.ClientSession{
						Id:     sessionID,
						Status: gateway.ClientSessionStatus_CLOSED,
					},
				},
			}

			err := agent.ControlStream.Send(disconnectMsg)
			if err != nil {
				s.Logger.Debug().
					Err(err).
					Str("session_id", sessionID).
					Str("agent_id", agentID).
					Msg("Failed to notify agent about client disconnection")
			}
		}
		agent.Mutex.Unlock()
	}

	// Update stats
	s.stats.ActiveSessions--

	// Keep session in memory for a while for stats
	go func() {
		time.Sleep(5 * time.Minute)
		s.mu.Lock()
		delete(s.sessions, sessionID)
		s.mu.Unlock()
	}()

	s.mu.Unlock()

	s.Logger.Info().
		Str("session_id", sessionID).
		Str("agent_id", agentID).
		Msg("Session closed")
}

// waitForTunnelConnection waits for a tunnel connection from an agent
func (s *GatewayServer) waitForTunnelConnection(agent *entities.Agent, tunnelID string, timeout time.Duration) net.Conn {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		select {
		case <-ticker.C:
			agent.Mutex.Lock()
			conn, exists := agent.Tunnels[tunnelID]
			if exists {
				delete(agent.Tunnels, tunnelID)
				agent.Mutex.Unlock()
				return conn
			}
			agent.Mutex.Unlock()
		}
	}

	return nil
}

// SelectAgent selects an available agent
func (s *GatewayServer) SelectAgent() *entities.Agent {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// TODO: Implement more sophisticated agent selection logic
	// For now, just return the first connected agent
	for _, agent := range s.agents {
		agent.Mutex.Lock()
		status := agent.Status
		hasControlStream := agent.ControlStream != nil
		agent.Mutex.Unlock()

		if status == gateway.AgentStatus_CONNECTED && hasControlStream {
			return agent
		}
	}

	return nil
}

// RegisterAgent implements the RegisterAgent RPC
func (s *GatewayServer) RegisterAgent(ctx context.Context, req *gateway.AgentRegistrationRequest) (*gateway.AgentRegistrationResponse, error) {
	s.Logger.Debug().
		Str("name", req.Name).
		Msg("Agent registration initiated")
	// Validate auth token
	if req.AuthToken != s.config.AuthToken {
		s.Logger.Warn().
			Str("name", req.Name).
			Msg("Agent registration failed: invalid auth token")

		return &gateway.AgentRegistrationResponse{
			Success:      false,
			ErrorMessage: "invalid auth token",
		}, nil
	}

	// Generate agent ID
	agentID := uuid.New().String()

	// Find an available port for the tunnel listener
	tunnelPort := s.findAvailableTunnelPort()

	// Create agent
	agent := &entities.Agent{
		ID:                 agentID,
		Name:               req.Name,
		Version:            req.Version,
		ConnectedAt:        time.Now(),
		Status:             gateway.AgentStatus_CONNECTED,
		TunnelListenerPort: tunnelPort,
		Tunnels:            make(map[string]net.Conn),
		Metadata:           req.Metadata,
		Stats: &gateway.AgentStats{
			AgentId:           agentID,
			ActiveSessions:    0,
			TotalSessions:     0,
			BytesSent:         0,
			BytesReceived:     0,
			ConnectionRate:    0,
			DisconnectionRate: 0,
		},
	}

	// Start tunnel listener
	tunnelAddr := fmt.Sprintf("%s:%d", s.config.ControlHost, tunnelPort)
	tunnelListener, err := net.Listen("tcp", tunnelAddr)
	if err != nil {
		s.Logger.Error().
			Err(err).
			Str("addr", tunnelAddr).
			Msg("Failed to start tunnel listener")

		return &gateway.AgentRegistrationResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to start tunnel listener: %v", err),
		}, nil
	}

	agent.TunnelListener = tunnelListener

	// Start goroutine to accept tunnel connections
	go s.acceptTunnelConnections(agent)

	// Add agent to map
	s.mu.Lock()
	s.agents[agentID] = agent
	s.stats.TotalAgents++
	s.stats.ActiveAgents++
	s.mu.Unlock()

	s.Logger.Info().
		Str("agent_id", agentID).
		Str("name", req.Name).
		Str("version", req.Version).
		Int("tunnel_port", tunnelPort).
		Msg("Agent registered")

	return &gateway.AgentRegistrationResponse{
		AgentId: agentID,
		Success: true,
	}, nil
}

// ControlChannel implements the ControlChannel RPC
func (s *GatewayServer) ControlChannel(stream gateway.GatewayService_ControlChannelServer) error {
	// First message should identify the agent
	msg, err := stream.Recv()
	if err != nil {
		return err
	}

	if msg.AgentId == "" {
		return fmt.Errorf("agent_id is required")
	}

	// Get agent
	s.mu.RLock()
	agent, exists := s.agents[msg.AgentId]
	s.mu.RUnlock()

	if !exists {
		return fmt.Errorf("agent not found: %s", msg.AgentId)
	}

	// Set agent's control stream
	agent.Mutex.Lock()
	agent.ControlStream = stream
	agent.Status = gateway.AgentStatus_CONNECTED
	agent.Mutex.Unlock()

	s.Logger.Info().
		Str("agent_id", agent.ID).
		Msg("Agent connected to control channel")

	// Send tunnel port to agent
	portMsg := &gateway.ControlMessage{
		Type:      gateway.ControlMessage_TUNNEL_PORT,
		AgentId:   agent.ID,
		Timestamp: timestamppb.Now(),
		Payload: &gateway.ControlMessage_TunnelPort{
			TunnelPort: &gateway.TunnelPort{
				Port: int32(agent.TunnelListenerPort),
			},
		},
	}

	if err := stream.Send(portMsg); err != nil {
		s.Logger.Error().
			Err(err).
			Str("agent_id", agent.ID).
			Msg("Failed to send tunnel port")

		agent.Mutex.Lock()
		agent.Status = gateway.AgentStatus_DISCONNECTED
		agent.ControlStream = nil
		agent.Mutex.Unlock()

		return err
	}

	// Handle control messages
	for {
		msg, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				s.Logger.Error().
					Err(err).
					Str("agent_id", agent.ID).
					Msg("Error receiving control message")
			}
			break
		}

		// Process message based on type
		switch msg.Type {
		case gateway.ControlMessage_HEARTBEAT:
			// Send heartbeat response
			heartbeatResp := &gateway.ControlMessage{
				Type:      gateway.ControlMessage_HEARTBEAT,
				AgentId:   agent.ID,
				Timestamp: timestamppb.Now(),
			}

			if err := stream.Send(heartbeatResp); err != nil {
				s.Logger.Error().
					Err(err).
					Str("agent_id", agent.ID).
					Msg("Failed to send heartbeat response")

				agent.Mutex.Lock()
				agent.Status = gateway.AgentStatus_DISCONNECTED
				agent.ControlStream = nil
				agent.Mutex.Unlock()

				return err
			}

		case gateway.ControlMessage_STATS:
			// Update agent stats
			if stats := msg.GetStats(); stats != nil {
				agent.Mutex.Lock()
				agent.Stats = stats
				agent.Mutex.Unlock()

				s.Logger.Debug().
					Str("agent_id", agent.ID).
					Int32("active_sessions", stats.ActiveSessions).
					Int64("bytes_sent", stats.BytesSent).
					Int64("bytes_received", stats.BytesReceived).
					Msg("Received agent stats")
			}

		case gateway.ControlMessage_ADMIN_COMMAND:
			// Process admin command
			// TODO: Implement admin command processing

		case gateway.ControlMessage_CERTIFICATE_RESPONSE:
			// Handle certificate response
			if cert := msg.GetCertificate(); cert != nil {
				// Get request timestamp from the message
				requestTimestamp := msg.Timestamp.AsTime().Format(time.RFC3339Nano)

				// Find response channel for this request
				agent.Mutex.Lock()
				respChan, respExists := agent.CertRespChannels[requestTimestamp]
				agent.Mutex.Unlock()

				if respExists {
					// Send certificate to the waiting handler
					select {
					case respChan <- cert:
						s.Logger.Debug().
							Str("agent_id", agent.ID).
							Str("cert_type", cert.Filename).
							Msg("Received certificate response")
					default:
						s.Logger.Warn().
							Str("agent_id", agent.ID).
							Str("cert_type", cert.Filename).
							Msg("Failed to deliver certificate response, channel full or closed")
					}
				} else {
					s.Logger.Warn().
						Str("agent_id", agent.ID).
						Str("timestamp", requestTimestamp).
						Msg("Received certificate response for unknown request")
				}
			}

		default:
			s.Logger.Debug().
				Str("agent_id", agent.ID).
				Int32("type", int32(msg.Type)).
				Msg("Received unknown control message type")
		}
	}

	agent.Mutex.Lock()
	agent.Status = gateway.AgentStatus_DISCONNECTED
	agent.ControlStream = nil
	agent.Mutex.Unlock()

	// Update stats
	s.mu.Lock()
	s.stats.ActiveAgents--
	s.mu.Unlock()

	s.Logger.Info().
		Str("agent_id", agent.ID).
		Msg("Agent disconnected from control channel")

	return nil
}

// GetAgents implements the GetAgents RPC
func (s *GatewayServer) GetAgents(ctx context.Context, req *gateway.Empty) (*gateway.AgentList, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	agents := make([]*gateway.Agent, 0, len(s.agents))

	for _, agent := range s.agents {
		agent.Mutex.Lock()
		agents = append(agents, &gateway.Agent{
			Id:          agent.ID,
			Name:        agent.Name,
			Version:     agent.Version,
			ConnectedAt: timestamppb.New(agent.ConnectedAt),
			Status:      agent.Status,
			Metadata:    agent.Metadata,
		})
		agent.Mutex.Unlock()
	}

	return &gateway.AgentList{
		Agents: agents,
	}, nil
}

// GetSessions implements the GetSessions RPC
func (s *GatewayServer) GetSessions(ctx context.Context, req *gateway.SessionListRequest) (*gateway.SessionList, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sessions := make([]*gateway.ClientSession, 0)

	for _, session := range s.sessions {
		if req.AgentId != "" && session.AgentID != req.AgentId {
			continue
		}

		session.Mutex.Lock()
		sessions = append(sessions, &gateway.ClientSession{
			Id:            session.ID,
			TunnelId:      session.TunnelID,
			ClientId:      session.ClientID,
			AgentId:       session.AgentID,
			ConnectedAt:   timestamppb.New(session.ConnectedAt),
			LastActivity:  timestamppb.New(session.LastActivity),
			BytesSent:     session.BytesSent,
			BytesReceived: session.BytesReceived,
			Status:        session.Status,
		})
		session.Mutex.Unlock()
	}

	return &gateway.SessionList{
		Sessions: sessions,
	}, nil
}

// DisconnectSession implements the DisconnectSession RPC
func (s *GatewayServer) DisconnectSession(ctx context.Context, req *gateway.DisconnectSessionRequest) (*gateway.Empty, error) {
	s.mu.RLock()
	session, exists := s.sessions[req.SessionId]
	s.mu.RUnlock()

	if !exists {
		return &gateway.Empty{}, fmt.Errorf("session not found: %s", req.SessionId)
	}

	s.closeSession(session)

	return &gateway.Empty{}, nil
}

// GetGatewayStats implements the GetGatewayStats RPC
func (s *GatewayServer) GetGatewayStats(ctx context.Context, req *gateway.Empty) (*gateway.GatewayStats, error) {
	s.stats.mu.Lock()
	stats := &gateway.GatewayStats{
		TotalAgents:        s.stats.TotalAgents,
		ActiveAgents:       s.stats.ActiveAgents,
		TotalSessions:      s.stats.TotalSessions,
		ActiveSessions:     s.stats.ActiveSessions,
		TotalBytesSent:     s.stats.TotalBytesSent,
		TotalBytesReceived: s.stats.TotalBytesReceived,
		CpuUsage:           s.stats.CPUUsage,
		MemoryUsage:        s.stats.MemoryUsage,
	}
	s.stats.mu.Unlock()

	return stats, nil
}

// findAvailableTunnelPort finds an available port for tunnel connections
func (s *GatewayServer) findAvailableTunnelPort() int {
	basePort := s.config.TunnelPortBase
	maxPort := basePort + 1000 // Try up to 1000 ports

	for port := basePort; port < maxPort; port++ {
		addr := fmt.Sprintf("%s:%d", s.config.ControlHost, port)
		listener, err := net.Listen("tcp", addr)
		if err == nil {
			listener.Close()
			return port
		}
	}

	// If no port is available, just return a random port and hope for the best
	return basePort + int(time.Now().Unix()%1000)
}

// acceptTunnelConnections accepts tunnel connections from the agent
func (s *GatewayServer) acceptTunnelConnections(agent *entities.Agent) {
	for {
		conn, err := agent.TunnelListener.Accept()
		if err != nil {
			s.Logger.Error().
				Err(err).
				Str("agent_id", agent.ID).
				Msg("Error accepting tunnel connection")
			break
		}

		go s.handleTunnelConnection(agent, conn)
	}

	// If we get here, the listener has been closed
	agent.Mutex.Lock()
	agent.Status = gateway.AgentStatus_DISCONNECTED

	// Close all tunnel connections
	for tunnelID, tunnelConn := range agent.Tunnels {
		tunnelConn.Close()
		delete(agent.Tunnels, tunnelID)
	}

	agent.Mutex.Unlock()

	s.Logger.Info().
		Str("agent_id", agent.ID).
		Msg("Tunnel listener closed")
}

// handleTunnelConnection handles a new tunnel connection
func (s *GatewayServer) handleTunnelConnection(agent *entities.Agent, conn net.Conn) {
	// Read tunnel ID from the connection
	buffer := make([]byte, 36) // UUID length

	// Set read deadline
	err := conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err != nil {
		s.Logger.Error().
			Err(err).
			Str("agent_id", agent.ID).
			Msg("Failed to set read deadline")
		conn.Close()
		return
	}

	n, err := conn.Read(buffer)
	if err != nil || n != 36 {
		s.Logger.Error().
			Err(err).
			Str("agent_id", agent.ID).
			Int("bytes_read", n).
			Msg("Error reading tunnel ID")
		conn.Close()
		return
	}

	// Reset read deadline
	err = conn.SetReadDeadline(time.Time{})
	if err != nil {
		s.Logger.Error().
			Err(err).
			Str("agent_id", agent.ID).
			Msg("Failed to reset read deadline")
		conn.Close()
		return
	}

	tunnelID := string(buffer[:n])

	// Store the tunnel connection
	agent.Mutex.Lock()
	agent.Tunnels[tunnelID] = conn
	agent.Mutex.Unlock()

	s.Logger.Info().
		Str("agent_id", agent.ID).
		Str("tunnel_id", tunnelID).
		Msg("Tunnel connection established")

	// The connection will be used by the client session, so we don't close it here
}

// collectStats collects system stats
func (s *GatewayServer) collectStats(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// TODO: Collect CPU and memory usage
			// For now, just log the number of active sessions
			s.mu.RLock()
			s.Logger.Info().
				Int("agents", len(s.agents)).
				Int("sessions", len(s.sessions)).
				Msg("Stats update")
			s.mu.RUnlock()
		}
	}
}
