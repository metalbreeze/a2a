#!/usr/bin/env bash
# Open an SSE connection and print every task the broker pushes.
# Requires: source ./agent.env   (produced by register.sh)
set -euo pipefail
: "${BROKER_URL:?run: source agent.env first}"
: "${AGENT_ID:?run: source agent.env first}"
: "${AGENT_TOKEN:?run: source agent.env first}"

echo "📡 listening on $BROKER_URL/agents/$AGENT_ID/inbox/stream (Ctrl+C to quit)"
curl -N \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -H 'Accept: text/event-stream' \
  "$BROKER_URL/agents/$AGENT_ID/inbox/stream"
