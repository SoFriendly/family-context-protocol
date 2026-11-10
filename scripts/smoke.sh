#!/bin/sh
# Release smoke test: first run, MCP over plain curl, the REST coordination loop,
# and "nothing left on the server" afterwards. Needs curl and python3 (JSON only).
#   ./scripts/smoke.sh [path/to/fcp]
set -eu
FCP=${1:-./fcp}
PORT=18181
DIR=$(mktemp -d)
BASE=http://127.0.0.1:$PORT
trap 'kill $PID 2>/dev/null || true; rm -rf "$DIR"' EXIT
j() { python3 -c "import sys,json; d=json.load(sys.stdin); print(eval('d'+sys.argv[1]))" "$1"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

"$FCP" serve --listen 127.0.0.1:$PORT --keys "$DIR/fcp-keys.json" >"$DIR/log" 2>&1 & PID=$!
sleep 1
HH=$(cat "$DIR/household-key.txt")
grep -q "$HH" "$DIR/log" && fail "household key leaked into non-terminal output"
curl -sf $BASE/healthz >/dev/null || fail healthz

mint() { curl -sf -X POST $BASE/v1/agents -H "authorization: Bearer $HH" -H 'content-type: application/json' -d "{\"name\":\"$1\"}"; }
J=$(mint "Jamie's Muse"); JK=$(echo "$J" | j '["agent_key"]'); JID=$(echo "$J" | j '["agent_id"]')
A=$(mint "Alex's Muse"); AK=$(echo "$A" | j '["agent_key"]'); AID=$(echo "$A" | j '["agent_id"]')

# --- MCP over curl ---
mcp() { curl -sf -X POST $BASE/mcp -H 'content-type: application/json' -H 'accept: application/json, text/event-stream' -H "authorization: Bearer $1" -d "$2"; }
mcp "$JK" '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"1"}}}' | j '["result"]["protocolVersion"]' | grep -q 2025-11-25 || fail initialize
mcp "$JK" '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null
[ "$(mcp "$JK" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | j '["result"]["tools"].__len__()')" = 6 ] || fail "tools/list"
mcp "$JK" "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"send_message\",\"arguments\":{\"to_agent_id\":\"$AID\",\"kind\":\"notice\",\"body\":\"hello over MCP\"}}}" >/dev/null
MID=$(mcp "$AK" '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"check_inbox","arguments":{}}}' | j '["result"]["structuredContent"]["messages"][0]["id"]')
mcp "$AK" "{\"jsonrpc\":\"2.0\",\"id\":5,\"method\":\"tools/call\",\"params\":{\"name\":\"check_inbox\",\"arguments\":{\"after\":$MID}}}" \
  | j '["result"]["structuredContent"]["messages"].__len__()' | grep -qx 0 || fail "MCP ack"
echo "ok   MCP: initialize -> tools/list -> send_message -> check_inbox -> check_inbox(after) empties it"

# --- REST-only coordination loop ---
rest() { curl -sf -X "$1" "$BASE$2" -H "authorization: Bearer $3" -H 'content-type: application/json' ${4:+-d "$4"}; }
TID=$(rest POST /v1/messages "$JK" "{\"to_agent_id\":\"$AID\",\"kind\":\"task\",\"body\":\"Add MCO parking (Garage C, \$264) to the trip plan\"}" | j '["id"]')
[ "$(rest GET /v1/inbox "$AK" | j '["messages"][0]["id"]')" = "$TID" ] || fail "Alex's Muse did not get the task"
rest POST /v1/messages "$AK" "{\"to_agent_id\":\"$JID\",\"kind\":\"reply\",\"re\":$TID,\"body\":\"Added to trip plan\"}" >/dev/null
rest GET "/v1/inbox?after=$TID" "$AK" >/dev/null
R=$(rest GET /v1/inbox "$JK" | python3 -c 'import sys,json; print(json.dumps([m for m in json.load(sys.stdin)["messages"] if m["kind"]!="system"][0]))')
[ "$(echo "$R" | j '["body"]')" = "Added to trip plan" ] || fail "Jamie's Muse did not get the reply"
[ "$(rest GET /v1/inbox "$JK" | j '["messages"][0]["kind"]')" = "system" ] || fail "no 'Alex's Muse joined' system notice"
rest GET "/v1/inbox?after=$(echo "$R" | j '["id"]')" "$JK" >/dev/null
echo "ok   REST: task -> Alex's Muse picks it up -> replies -> Jamie's Muse gets 'Added to trip plan'"

[ "$(rest GET /v1/agents "$HH" | python3 -c 'import sys,json; print(sum(a["pending"] for a in json.load(sys.stdin)["agents"]))')" = 0 ] || fail "server still holds messages"
grep -q "trip plan" "$DIR/fcp-keys.json" && fail "message content on disk"
echo "ok   nothing left: 0 pending messages, no content on disk"
echo PASS
