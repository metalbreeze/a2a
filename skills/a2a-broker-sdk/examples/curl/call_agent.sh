#!/usr/bin/env bash
# Discover a provider by skill tag and send it a task.
#   ./call_agent.sh <skill_tag> "message text"
set -euo pipefail

BROKER="${BROKER_URL:-http://www.cybertron.studio/a2a}"
tag="${1:?usage: $0 <skill_tag> <message>}"
text="${2:-hi}"

echo "🔎 discovering agent with skill=$tag ..."
provider_id=$(curl -sS "$BROKER/registry/agents?skill=$tag&available=now" \
  | python3 -c 'import json,sys;arr=json.load(sys.stdin);print(arr[0]["agent_id"] if arr else "",end="")')
if [ -z "$provider_id" ]; then
  echo "❌ no online provider with that skill"; exit 1
fi
echo "   -> $provider_id"

send_resp=$(curl -sS -X POST "$BROKER/agents/$provider_id/a2a" \
  -H 'Content-Type: application/json' \
  -d "$(cat <<EOF
{"jsonrpc":"2.0","id":"1","method":"message/send",
 "params":{"message":{"messageId":"m-$(date +%s)","role":"ROLE_USER",
                      "parts":[{"text":"$text"}]}}}
EOF
)")
task_id=$(echo "$send_resp" | python3 -c 'import json,sys;print(json.load(sys.stdin)["result"]["id"])')
echo "📤 sent task $task_id"

echo "⏳ waiting for reply..."
for _ in $(seq 1 60); do
  sleep 1
  state=$(curl -sS -X POST "$BROKER/agents/$provider_id/a2a" \
    -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":\"2\",\"method\":\"tasks/get\",\"params\":{\"id\":\"$task_id\"}}" \
    | python3 -c 'import json,sys;r=json.load(sys.stdin)["result"];print(r["status"]["state"]);import sys;sys.exit(0 if r["status"]["state"] in ("TASK_STATE_COMPLETED","TASK_STATE_FAILED","TASK_STATE_REJECTED") else 1)') \
    && { echo "$state"; break; } || true
done

echo ""
echo "--- full final task ---"
curl -sS -X POST "$BROKER/agents/$provider_id/a2a" \
  -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":\"3\",\"method\":\"tasks/get\",\"params\":{\"id\":\"$task_id\"}}"
echo ""
