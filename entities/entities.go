package entities

import (
	"github.com/xdire/dtc-proto/gol/gateway"
	"net"
	"sync"
	"time"
)

// Config holds configuration for the LB Gateway
type Config struct {
	// Public-facing service (where clients connect)
	PublicHost string
	PublicPort int

	// Control service (where agents connect)
	ControlHost string
	ControlPort int

	// Base port for tunnel connections
	TunnelPortBase int

	// TLS configuration
	TLSEnabled  bool
	TLSCertFile string
	TLSKeyFile  string

	// Authentication
	AuthToken string

	// Logging
	LogLevel string

	// Port for the HTTP certificate server
	HTTPPort int
}

// Agent represents a connected chat server
type Agent struct {
	ID                 string
	Name               string
	Version            string
	ConnectedAt        time.Time
	Status             gateway.AgentStatus
	ControlStream      gateway.GatewayService_ControlChannelServer
	TunnelListenerPort int
	TunnelListener     net.Listener
	Tunnels            map[string]net.Conn
	Metadata           map[string]string
	Stats              *gateway.AgentStats
	Mutex              sync.Mutex
	CertRespChannels   map[string]chan<- *gateway.Certificate
	CertErrChannels    map[string]chan<- error
}

// ClientSession represents a client connected to the LB Gateway
type ClientSession struct {
	ID            string
	ClientConn    net.Conn
	TunnelConn    net.Conn
	AgentID       string
	ClientID      string
	TunnelID      string
	ConnectedAt   time.Time
	LastActivity  time.Time
	BytesSent     int64
	BytesReceived int64
	Status        gateway.ClientSessionStatus
	Mutex         sync.Mutex
}
