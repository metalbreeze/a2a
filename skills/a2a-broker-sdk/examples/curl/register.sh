#!/usr/bin/env bash
# Register a new agent with the broker and save the credentials to agent.env
set -euo pipefail

BROKER="${BROKER_URL:-http://www.cybertron.studio/a2a}"
NAME="${NAME:-my-agent}"

resp=$(curl -sS -X POST "$BROKER/registry/agents" \
  -H 'Content-Type: application/json' \
  -d @- <<EOF
{
  "name": "$NAME",
  "description": "A demo agent",
  "version": "0.1.0",
  "mode": "realtime",
  "card": {
    "name": "$NAME",
    "description": "A demo agent",
    "version": "0.1.0",
    "protocolVersion": "0.3.0",
    "capabilities": {
      "streaming": false,
      "pushNotifications": false,
      "stateTransitionHistory": false
    },
    "defaultInputModes":  ["text/plain"],
    "defaultOutputModes": ["text/plain"],
    "skills": [
      {
        "id": "greet",
        "name": "Greet",
        "description": "Says hello.",
        "tags": ["hello", "demo"]
      }
    ]
  }
}
EOF
)

echo "$resp"

agent_id=$(echo "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin)["agent_id"])')
token=$(echo "$resp"    | python3 -c 'import json,sys;print(json.load(sys.stdin)["token"])')

cat > agent.env <<EOF
export BROKER_URL="$BROKER"
export AGENT_ID="$agent_id"
export AGENT_TOKEN="$token"
EOF

echo ""
echo "✅ credentials written to agent.env — source it before running listen.sh / reply.sh"
