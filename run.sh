#!/bin/bash
# Enhanced run.sh script with YAML config and TLS support

set -e

# Load configuration from YAML if available
CONFIG_FILE="${1:-config.yaml}"

if command -v yq &> /dev/null && [ -f "$CONFIG_FILE" ]; then
    echo "Loading configuration from $CONFIG_FILE..."
    PUBLIC_HOST=$(yq '.gateway.public_host' "$CONFIG_FILE")
    PUBLIC_PORT=$(yq '.gateway.public_port' "$CONFIG_FILE")
    CONTROL_HOST=$(yq '.gateway.control_host' "$CONFIG_FILE")
    CONTROL_PORT=$(yq '.gateway.control_port' "$CONFIG_FILE")
    TUNNEL_PORT_BASE=$(yq '.gateway.tunnel_port_base' "$CONFIG_FILE")
    TLS_ENABLED=$(yq '.gateway.tls.enabled' "$CONFIG_FILE")
    TLS_CERT=$(yq '.gateway.tls.cert_path' "$CONFIG_FILE")
    TLS_KEY=$(yq '.gateway.tls.key_path' "$CONFIG_FILE")
    HTTP_ENABLED=$(yq '.gateway.http_cert_server.enabled' "$CONFIG_FILE")
    HTTP_PORT=$(yq '.gateway.http_cert_server.port' "$CONFIG_FILE")
    LOG_LEVEL=$(yq '.gateway.log_level' "$CONFIG_FILE")
    AUTH_TOKEN=$(yq '.gateway.auth_token' "$CONFIG_FILE")

    # Check if TLS cert and key exist, generate if necessary
    if [ "$TLS_ENABLED" = "true" ]; then
        CERT_DIR=$(dirname "$TLS_CERT")
        mkdir -p "$CERT_DIR"

        if [ ! -f "$TLS_CERT" ] || [ ! -f "$TLS_KEY" ]; then
            echo "Generating TLS certificates..."
            CERT_YEARS=$(yq '.gateway.tls.longevity_years' "$CONFIG_FILE")
            CERT_DAYS=$((CERT_YEARS * 365))
            openssl req -x509 -nodes -newkey rsa:4096 -keyout "$TLS_KEY" -out "$TLS_CERT" -days "$CERT_DAYS" -subj "/CN=dtc-gateway/O=DTC Chat/C=US"
            chmod 600 "$TLS_KEY"
            echo "Certificates generated at $TLS_CERT and $TLS_KEY"
        fi
    fi
else
    echo "YAML configuration not found or 'yq' not installed. Using default values."
    # Default configuration
    PUBLIC_HOST="0.0.0.0"
    PUBLIC_PORT=50053
    CONTROL_HOST="0.0.0.0"
    CONTROL_PORT=50054
    TUNNEL_PORT_BASE=60000
    TLS_ENABLED=false
    HTTP_ENABLED=true
    HTTP_PORT=8443
    LOG_LEVEL="info"
fi

# Generate auth token if not provided
if [ -z "$AUTH_TOKEN" ]; then
    AUTH_TOKEN=$(openssl rand -hex 32)
    echo "Generated AUTH_TOKEN: $AUTH_TOKEN"
fi

# Build command
CMD="go run main.go --public-host=$PUBLIC_HOST --public-port=$PUBLIC_PORT --control-host=$CONTROL_HOST --control-port=$CONTROL_PORT --tunnel-port-base=$TUNNEL_PORT_BASE --auth-token=$AUTH_TOKEN --log-level=$LOG_LEVEL"

# Add HTTP cert server parameters if enabled
if [ "$HTTP_ENABLED" = "true" ]; then
    CMD="$CMD --http-port=$HTTP_PORT"
else
    CMD="$CMD --http-port=0"  # 0 disables the HTTP server
fi

# Add TLS parameters if enabled
if [ "$TLS_ENABLED" = "true" ]; then
    CMD="$CMD --tls --tls-cert=$TLS_CERT --tls-key=$TLS_KEY"
fi

echo "Running gateway with command: $CMD"
echo "---------------------------------------------"
echo "Public endpoint: $PUBLIC_HOST:$PUBLIC_PORT"
echo "Control endpoint: $CONTROL_HOST:$CONTROL_PORT"
echo "TLS enabled: $TLS_ENABLED"
echo "---------------------------------------------"
echo "To connect your chat server, use:"
echo "--enable-gateway=true"
echo "--gateway-addr=localhost:$CONTROL_PORT"
echo "--gateway-auth-token=$AUTH_TOKEN"
echo "---------------------------------------------"

# Save connection details to a file for reference
echo "# DTC Gateway Connection Details" > gateway-connection.txt
echo "PUBLIC_HOST=$PUBLIC_HOST" >> gateway-connection.txt
echo "PUBLIC_PORT=$PUBLIC_PORT" >> gateway-connection.txt
echo "CONTROL_PORT=$CONTROL_PORT" >> gateway-connection.txt
echo "AUTH_TOKEN=$AUTH_TOKEN" >> gateway-connection.txt
echo "TLS_ENABLED=$TLS_ENABLED" >> gateway-connection.txt
echo "" >> gateway-connection.txt
echo "# Connect your chat server with:" >> gateway-connection.txt
echo "--enable-gateway=true" >> gateway-connection.txt
echo "--gateway-addr=localhost:$CONTROL_PORT" >> gateway-connection.txt
echo "--gateway-auth-token=$AUTH_TOKEN" >> gateway-connection.txt
echo "--gateway-use-tls=$TLS_ENABLED" >> gateway-connection.txt
if [ "$TLS_ENABLED" = "true" ]; then
    echo "--gateway-tls-insecure=true  # For local development only" >> gateway-connection.txt
fi

# Execute the command
eval "$CMD"