#!/bin/bash
# Start Serene Glow demo bot
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PICOCLAW_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
PICOCLAW_BIN="$PICOCLAW_DIR/build/picoclaw"

# Build picoclaw if binary doesn't exist
if [ ! -f "$PICOCLAW_BIN" ]; then
  echo "Building picoclaw..."
  cd "$PICOCLAW_DIR"
  make build
fi

# Verify binary exists
if [ ! -f "$PICOCLAW_BIN" ]; then
  echo "ERROR: picoclaw binary not found at $PICOCLAW_BIN"
  echo "Run: cd $PICOCLAW_DIR && make build"
  exit 1
fi

CONFIG="$SCRIPT_DIR/config.json"
WEB_SERVER="$SCRIPT_DIR/web-chat/server.js"

# Check if port 18810 is already in use
if lsof -iTCP:18810 -sTCP:LISTEN -P -n > /dev/null 2>&1; then
  echo "WARNING: Port 18810 already in use. Picoclaw may already be running."
fi

echo ""
echo "  ✨ Serene Glow Salon & Spa — Demo Bot"
echo "  ────────────────────────────────────"
echo "  AI Backend: Gemini 2.0 Flash Lite"
echo "  Gateway:    http://localhost:18810"
echo "  Web Chat:   http://localhost:3000"
echo ""

# Start picoclaw in background
echo "Starting picoclaw gateway..."
PICOCLAW_CONFIG="$CONFIG" "$PICOCLAW_BIN" gateway &
PICOCLAW_PID=$!

# Wait for picoclaw to start
sleep 2

# Check picoclaw started
if ! kill -0 $PICOCLAW_PID 2>/dev/null; then
  echo "ERROR: picoclaw failed to start"
  exit 1
fi

echo "Picoclaw running (PID: $PICOCLAW_PID)"
echo ""

# Start web server
echo "Starting web chat server..."
node "$WEB_SERVER" &
WEB_PID=$!

sleep 1

if ! kill -0 $WEB_PID 2>/dev/null; then
  echo "ERROR: Web server failed to start"
  kill $PICOCLAW_PID 2>/dev/null
  exit 1
fi

echo ""
echo "  Demo is live at http://localhost:3000"
echo "  Press Ctrl+C to stop both servers."
echo ""

# Cleanup on exit
trap "echo ''; echo 'Stopping...'; kill $PICOCLAW_PID $WEB_PID 2>/dev/null; exit 0" INT TERM

# Wait
wait $PICOCLAW_PID $WEB_PID
