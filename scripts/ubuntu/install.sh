#!/bin/bash
# Standalone Gateway Installation Script for Ubuntu 24.04
# This script installs and configures the DTC Gateway service on a Digital Ocean droplet

set -e

# Colors for terminal output
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

echo -e "${GREEN}=== DTC Chat Load Balancer Gateway Setup ===${NC}"
echo "This script will set up the Load Balancer Gateway on this server."

# Check if the script is run as root
if [ "$EUID" -ne 0 ]; then
  echo -e "${RED}Please run as root${NC}"
  exit 1
fi

# Get GitHub token for private repo access
read -p "Enter your GitHub Personal Access Token: " GITHUB_TOKEN
if [ -z "$GITHUB_TOKEN" ]; then
  echo -e "${RED}GitHub token is required to access private repositories${NC}"
  exit 1
fi

# Configuration variables (can be customized)
PUBLIC_PORT=50053
CONTROL_PORT=50054
TUNNEL_PORT_BASE=60000
LOG_LEVEL="info"
HTTP_PORT=8443
TLS_ENABLED=true
TLS_LONGEVITY_YEARS=5

# Generate a random auth token
AUTH_TOKEN=$(openssl rand -hex 32)

# Install dependencies
echo -e "${YELLOW}Installing system dependencies...${NC}"
apt-get update
apt-get install -y golang-go git ufw make curl jq

# Create service directories
echo -e "${YELLOW}Creating service directories...${NC}"
mkdir -p /opt/dtc-gateway
mkdir -p /etc/dtc-gateway/certs
cd /opt/dtc-gateway

# Configure Git to use the token for authentication
echo -e "${YELLOW}Configuring Git authentication...${NC}"
git config --global credential.helper store
echo "https://$GITHUB_TOKEN:x-oauth-basic@github.com" > ~/.git-credentials
chmod 600 ~/.git-credentials

# Configure Go to use the GitHub token for private modules
echo -e "${YELLOW}Configuring Go for private module access...${NC}"
git config --global url."https://$GITHUB_TOKEN:x-oauth-basic@github.com/".insteadOf "https://github.com/"
cat > ~/.netrc <<EOL
machine github.com
login $GITHUB_TOKEN
password x-oauth-basic
EOL
chmod 600 ~/.netrc

# Go environment setup
mkdir -p $HOME/go/bin
export GOPATH=$HOME/go
export PATH=$PATH:$GOPATH/bin

# Clone the repositories
echo -e "${YELLOW}Cloning repositories...${NC}"
if ! git clone https://github.com/xdire/dtc-gateway.git; then
  echo -e "${RED}Failed to clone gateway repository. Check your GitHub token and permissions.${NC}"
  exit 1
fi

cd dtc-gateway

# Generate TLS certificates if enabled
if [ "$TLS_ENABLED" = "true" ]; then
  echo -e "${YELLOW}Generating TLS certificates...${NC}"
  mkdir -p /etc/dtc-gateway/certs
  CERT_DAYS=$((TLS_LONGEVITY_YEARS * 365))
  openssl req -x509 -nodes -newkey rsa:4096 \
    -keyout /etc/dtc-gateway/certs/server.key \
    -out /etc/dtc-gateway/certs/server.crt \
    -days $CERT_DAYS \
    -subj "/CN=dtc-gateway/O=DTC Chat/C=US"
  chmod 600 /etc/dtc-gateway/certs/server.key

  TLS_ARGS="--tls --tls-cert=/etc/dtc-gateway/certs/server.crt --tls-key=/etc/dtc-gateway/certs/server.key"
else
  TLS_ARGS=""
fi

# Build the gateway
echo -e "${YELLOW}Building gateway service...${NC}"
if [ -f "Makefile" ]; then
  echo "Using Makefile to build..."
  make build
else
  echo "Building manually..."
  go mod tidy
  go build -o dtc-gateway
fi

# Create systemd service
echo -e "${YELLOW}Creating systemd service...${NC}"
cat > /etc/systemd/system/dtc-gateway.service <<EOL
[Unit]
Description=DTC Chat Load Balancer Gateway
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/dtc-gateway/dtc-gateway
ExecStart=/opt/dtc-gateway/dtc-gateway/dtc-gateway \\
  --public-host=0.0.0.0 \\
  --public-port=${PUBLIC_PORT} \\
  --control-host=0.0.0.0 \\
  --control-port=${CONTROL_PORT} \\
  --tunnel-port-base=${TUNNEL_PORT_BASE} \\
  --auth-token=${AUTH_TOKEN} \\
  --log-level=${LOG_LEVEL} \\
  --http-port=${HTTP_PORT} \\
  ${TLS_ARGS}
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOL

# Configure firewall
echo -e "${YELLOW}Configuring firewall...${NC}"
ufw allow ssh
ufw allow ${PUBLIC_PORT}/tcp
ufw allow ${CONTROL_PORT}/tcp
ufw allow ${TUNNEL_PORT_BASE}:$((TUNNEL_PORT_BASE+100))/tcp
ufw allow ${HTTP_PORT}/tcp
ufw --force enable

# Enable and start the service
echo -e "${YELLOW}Starting gateway service...${NC}"
systemctl daemon-reload
systemctl enable dtc-gateway
systemctl start dtc-gateway

# Get server's public IP
PUBLIC_IP=$(curl -s icanhazip.com)

# Save connection details
echo -e "${YELLOW}Saving connection details...${NC}"
cat > /opt/dtc-gateway/connection.txt <<EOL
# DTC Gateway Connection Details
PUBLIC_HOST=${PUBLIC_IP}
PUBLIC_PORT=${PUBLIC_PORT}
CONTROL_PORT=${CONTROL_PORT}
AUTH_TOKEN=${AUTH_TOKEN}
TLS_ENABLED=${TLS_ENABLED}
HTTP_PORT=${HTTP_PORT}

# Connect your chat server with:
--enable-gateway=true
--gateway-addr=${PUBLIC_IP}:${CONTROL_PORT}
--gateway-auth-token=${AUTH_TOKEN}
EOL

if [ "$TLS_ENABLED" = "true" ]; then
  echo "--gateway-use-tls=true" >> /opt/dtc-gateway/connection.txt
  echo "--gateway-tls-insecure=true  # Remove this in production if using proper certificates" >> /opt/dtc-gateway/connection.txt
fi

chmod 600 /opt/dtc-gateway/connection.txt

# Check service status
STATUS=$(systemctl is-active dtc-gateway)
if [ "$STATUS" = "active" ]; then
  echo -e "${GREEN}Gateway service is running!${NC}"
else
  echo -e "${RED}Gateway service failed to start. Check logs with: journalctl -u dtc-gateway${NC}"
fi

echo -e "${GREEN}=== Setup Complete ===${NC}"
echo -e "Gateway service is running on ports:"
echo -e "  Public endpoint: ${PUBLIC_IP}:${PUBLIC_PORT}"
echo -e "  Control endpoint: ${PUBLIC_IP}:${CONTROL_PORT}"
echo -e "  HTTP cert server: ${PUBLIC_IP}:${HTTP_PORT}"
echo -e "${YELLOW}AUTH TOKEN:${NC} ${AUTH_TOKEN}"
echo -e "${YELLOW}TLS ENABLED:${NC} ${TLS_ENABLED}"
echo -e "${YELLOW}CONNECTION DETAILS:${NC} /opt/dtc-gateway/connection.txt"
echo -e ""
echo "To connect your chat server, run it with these flags:"
echo "  --enable-gateway=true"
echo "  --gateway-addr=${PUBLIC_IP}:${CONTROL_PORT}"
echo "  --gateway-auth-token=${AUTH_TOKEN}"
if [ "$TLS_ENABLED" = "true" ]; then
  echo "  --gateway-use-tls=true"
  echo "  --gateway-tls-insecure=true  # For testing only, remove in production"
fi

# Optional: Clean up GitHub credentials
echo -e "${YELLOW}Do you want to clean up GitHub credentials? (Recommended for security) [y/N]${NC}"
read -p "This may affect future updates to the service: " CLEANUP

if [[ $CLEANUP == "y" || $CLEANUP == "Y" ]]; then
  echo -e "${YELLOW}Cleaning up GitHub credentials...${NC}"
  rm ~/.git-credentials
  git config --global --unset credential.helper
  rm ~/.netrc
  git config --global --unset url."https://$GITHUB_TOKEN:x-oauth-basic@github.com/".insteadOf
  echo -e "${GREEN}Credentials cleaned up.${NC}"
fi
