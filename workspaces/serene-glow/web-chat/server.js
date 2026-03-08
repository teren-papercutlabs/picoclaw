#!/usr/bin/env node
/**
 * Serene Glow Demo — Web Chat Server
 *
 * Serves the chat UI and proxies WebSocket connections to picoclaw.
 *
 * Usage:
 *   node server.js [--port 3000] [--picoclaw ws://localhost:18810]
 */

const http = require('http');
const fs = require('fs');
const path = require('path');
const net = require('net');

// Parse CLI args
const args = process.argv.slice(2);
let PORT = 3000;
let PICOCLAW_WS = 'ws://localhost:18810';

for (let i = 0; i < args.length; i++) {
  if (args[i] === '--port' && args[i + 1]) PORT = parseInt(args[i + 1]);
  if (args[i] === '--picoclaw' && args[i + 1]) PICOCLAW_WS = args[i + 1];
}

// Resolve picoclaw host/port from WS URL
const picoclawUrl = new URL(PICOCLAW_WS);
const PICOCLAW_HOST = picoclawUrl.hostname;
const PICOCLAW_PORT = parseInt(picoclawUrl.port) || 18810;

// MIME types
const MIME = {
  '.html': 'text/html',
  '.js': 'application/javascript',
  '.css': 'text/css',
  '.json': 'application/json',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
};

const server = http.createServer((req, res) => {
  // Only serve static files (no WebSocket upgrade here)
  let filePath = req.url === '/' ? '/index.html' : req.url;
  filePath = path.join(__dirname, filePath);

  fs.readFile(filePath, (err, data) => {
    if (err) {
      res.writeHead(404);
      res.end('Not found');
      return;
    }
    const ext = path.extname(filePath);
    const contentType = MIME[ext] || 'application/octet-stream';
    res.writeHead(200, { 'Content-Type': contentType });
    res.end(data);
  });
});

// WebSocket proxy: forward upgrade requests to picoclaw
server.on('upgrade', (req, clientSocket, head) => {
  const targetSocket = net.createConnection(PICOCLAW_PORT, PICOCLAW_HOST, () => {
    // Rebuild the HTTP upgrade request
    const requestLine = `${req.method} ${req.url} HTTP/${req.httpVersion}\r\n`;
    const headers = Object.entries(req.headers)
      .map(([k, v]) => `${k}: ${v}`)
      .join('\r\n');
    const upgradeRequest = requestLine + headers + '\r\n\r\n';

    targetSocket.write(upgradeRequest);
    if (head && head.length > 0) targetSocket.write(head);

    targetSocket.pipe(clientSocket);
    clientSocket.pipe(targetSocket);
  });

  targetSocket.on('error', (err) => {
    console.error('[proxy] picoclaw connection error:', err.message);
    clientSocket.destroy();
  });

  clientSocket.on('error', (err) => {
    console.error('[proxy] client socket error:', err.message);
    targetSocket.destroy();
  });
});

server.listen(PORT, () => {
  console.log(`Serene Glow demo server running at http://localhost:${PORT}`);
  console.log(`Proxying WebSocket to picoclaw at ${PICOCLAW_WS}`);
  console.log('');
  console.log('To start picoclaw separately:');
  console.log(`  cd ~/pcl-dev/picoclaw`);
  console.log(`  ./build/picoclaw gateway --config workspaces/serene-glow/config.json`);
});
