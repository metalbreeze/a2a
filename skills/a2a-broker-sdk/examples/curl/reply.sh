#!/usr/bin/env bash
# Post a result for a task you just received on the SSE stream.
#   ./reply.sh <task_id> "reply text"
set -euo pipefail
: "${BROKER_URL:?run: source agent.env first}"
: "${AGENT_ID:?run: source agent.env first}"
: "${AGENT_TOKEN:?run: source agent.env first}"

task_id="${1:?usage: $0 <task_id> <reply text>}"
text="${2:-hello}"

curl -sS -X POST \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "$(cat <<EOF
{
  "state": "TASK_STATE_COMPLETED",
  "message": {
    "messageId": "reply-$task_id",
    "role": "ROLE_AGENT",
    "parts": [ { "text": "$text" } ]
  }
}
EOF
)" \
  "$BROKER_URL/agents/$AGENT_ID/tasks/$task_id/result"
echo ""
