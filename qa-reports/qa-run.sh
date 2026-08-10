#!/bin/bash
# QA runner: starts mock upstream + relay (given settings), runs curl tests.
# Usage: qa-run.sh <settings.json> [--no-synth]
set -u
ROOT=/root/ocfreelay-go
QA=$ROOT/qa-reports
SETTINGS=$1
shift
PORT=9901
REL_PID=""

log()  { echo "[QA] $*"; }
start() {
  rm -f $QA/mock-requests.log
  python3 $QA/mock_upstream.py 9902 > $QA/mock-upstream.log 2>&1 &
  MOCK_PID=$!
  sleep 0.6
  OCFREELAY_SETTINGS_PATH=$SETTINGS OCFREELAY_STATS_PATH=$QA/stats.json \
    $ROOT/dist/relay > $QA/relay.log 2>&1 &
  REL_PID=$!
  sleep 1
  for i in 1 2 3 4 5; do
    curl -s -o /dev/null http://127.0.0.1:$PORT/health && return 0
    sleep 0.5
  done
  log "FAIL: relay did not come up"; cat $QA/relay.log; exit 1
}
stop() {
  [ -n "$REL_PID" ] && kill $REL_PID 2>/dev/null
  kill $MOCK_PID 2>/dev/null
  sleep 0.3
}
reqs() { grep -c '"path"' $QA/mock-requests.log 2>/dev/null || echo 0; }

start

# ---------- /health ----------
code=$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:$PORT/health)
[ "$code" = "200" ] && log "PASS /health 200" || log "FAIL /health -> $code"

# ---------- /v1/models (usage-less JSON) ----------
out=$(curl -s -w "|%{http_code}|%{size_download}" http://127.0.0.1:$PORT/v1/models)
body=${out%|*}; rest=${out##*|}
code=${rest%|*}; size=${rest##*|}
case "$body" in
  *mock-model*) log "PASS /v1/models body intact ($size bytes, code $code)" ;;
  *) log "FAIL /v1/models body empty/truncated: size=$size code=$code body='$body'" ;;
esac

# ---------- query passthrough ----------
curl -s -o /dev/null "http://127.0.0.1:$PORT/v1/models?foo=1&bar=hello%20world"
last=$(tail -1 $QA/mock-requests.log)
echo "$last" | grep -q '"path": "/v1/models?foo=1&bar=hello world"' && log "PASS query preserved" || log "FAIL query: $last"

# ---------- non-stream chat + usage + client_metadata strip ----------
out=$(curl -s -w "|%{http_code}" -X POST http://127.0.0.1:$PORT/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"m1","client_metadata":{"ip":"1.2.3.4"},"messages":[{"role":"user","content":"hi"}]}')
code=${out##*|}
echo "$out" | grep -q '"pong"' && log "PASS chat non-stream body (code $code)" || log "FAIL chat body: $out"
last=$(tail -1 $QA/mock-requests.log)
echo "$last" | grep -q '"client_metadata"' && log "FAIL client_metadata NOT stripped" || log "PASS client_metadata stripped upstream"
echo "$last" | grep -q '"model":"m1"' && log "PASS other fields kept" || log "FAIL fields lost: $last"

# ---------- body passthrough: no client_metadata -> byte-identical ----------
body_in='{"model":"m1","messages":[{"role":"user","content":"hi"}]}'
curl -s -o /dev/null -X POST http://127.0.0.1:$PORT/v1/chat/completions -H 'Content-Type: application/json' -d "$body_in"
last=$(tail -1 $QA/mock-requests.log)
echo "$last" | grep -qF "$body_in" && log "PASS body without client_metadata unchanged" || log "FAIL body changed: $last"

# ---------- body passthrough: non-JSON ----------
curl -s -o /dev/null -X POST http://127.0.0.1:$PORT/v1/chat/completions -H 'Content-Type: text/plain' --data-binary 'raw non-json payload'
last=$(tail -1 $QA/mock-requests.log)
echo "$last" | grep -qF 'raw non-json payload' && log "PASS non-JSON body passthrough" || log "FAIL non-json: $last"

# ---------- body: JSON array (valid json, not object) ----------
curl -s -o /dev/null -X POST http://127.0.0.1:$PORT/v1/chat/completions -H 'Content-Type: application/json' -d '[1,2,{"a":1}]'
last=$(tail -1 $QA/mock-requests.log)
echo "$last" | grep -qF '[1,2,{"a":1}]' && log "PASS JSON-array body passthrough" || log "FAIL array: $last"

# ---------- SSE streaming ----------
sse=$(curl -s -N -X POST "http://127.0.0.1:$PORT/v1/chat/completions?stream=true" -H 'Content-Type: application/json' -d '{"model":"m1","messages":[]}')
echo "$sse" | grep -q 'data: \[DONE\]' && log "PASS SSE stream complete" || log "FAIL SSE: '$sse'"

# ---------- headers: allow-list forward ----------
curl -s -o /dev/null -X POST http://127.0.0.1:$PORT/v1/chat/completions \
  -H 'Content-Type: application/json' -H 'X-OpenCode-Client: my-agent' \
  -H 'X-OpenCode-Project: proj-x' -H 'X-Title: hello' \
  -H 'X-Custom-Evil: drop-me' -H 'Cookie: secret=1' \
  -H 'X-Forwarded-For: 9.9.9.9' -H 'User-Agent: curl-qa/1.0' \
  -d '{"model":"m1","messages":[]}'
last=$(tail -1 $QA/mock-requests.log)
ok=1
echo "$last" | grep -qi '"x-opencode-client": "my-agent"' || { log "FAIL x-opencode-client missing"; ok=0; }
echo "$last" | grep -qi '"x-opencode-project": "proj-x"' || { log "FAIL x-opencode-project missing"; ok=0; }
echo "$last" | grep -qi '"x-title": "hello"' || { log "FAIL x-title missing"; ok=0; }
echo "$last" | grep -qi '"user-agent": "curl-qa/1.0"' || { log "FAIL client UA not forwarded"; ok=0; }
echo "$last" | grep -qi 'cookie' && { log "FAIL Cookie leaked"; ok=0; }
echo "$last" | grep -qi 'x-custom-evil' && { log "FAIL X-Custom-Evil leaked"; ok=0; }
echo "$last" | grep -qi 'x-forwarded-for' && { log "FAIL X-Forwarded-For leaked"; ok=0; }
echo "$last" | grep -qi '"authorization": "Bearer key-w1"' || { log "FAIL worker key not in Authorization"; ok=0; }
[ $ok = 1 ] && log "PASS header allow-list + worker Authorization"

# ---------- session affinity synthesis ----------
curl -s -o /dev/null -X POST http://127.0.0.1:$PORT/v1/chat/completions \
  -H 'Content-Type: application/json' -H 'X-Session-Id: sess-affinity-1' \
  -d '{"model":"m1","messages":[]}'
last=$(tail -1 $QA/mock-requests.log)
echo "$last" | grep -qi '"x-opencode-session": "sess-affinity-1"' && log "PASS session affinity synthesized" || log "FAIL affinity: $last"

# ---------- CLI synthesis (no identity headers at all) ----------
curl -s -o /dev/null -X POST http://127.0.0.1:$PORT/v1/chat/completions -H 'Content-Type: application/json' -d '{"model":"m1","messages":[]}'
last=$(tail -1 $QA/mock-requests.log)
ok=1
echo "$last" | grep -qi '"user-agent": "opencode-cli/1.0.0"' || { log "FAIL CLI UA not synthesized"; ok=0; }
echo "$last" | grep -qi '"x-opencode-client": "cli"' || { log "FAIL x-opencode-client not synthesized"; ok=0; }
echo "$last" | grep -qi '"x-opencode-project": "default"' || { log "FAIL x-opencode-project not synthesized"; ok=0; }
sess=$(echo "$last" | grep -o '"x-opencode-session": "[^"]*"' | head -1)
req=$(echo "$last" | grep -o '"x-opencode-request": "[^"]*"' | head -1)
case "$sess$req" in
  *"x-opencode-session":*"x-opencode-request":*) log "PASS CLI synth session+request UUIDs" ;;
  *) log "FAIL CLI uuid: $last" ;;
esac
[ $ok = 1 ] && log "PASS CLI synthesis"

# ---------- retry: 429 free-limit -> ban 24h, next worker succeeds ----------
n0=$(reqs)
out=$(curl -s -w "|%{http_code}" -X POST http://127.0.0.1:$PORT/v1/chat/completions \
  -H 'Content-Type: application/json' -d '{"model":"m1","messages":[],"x-trigger-429free":1}')
code=${out##*|}
n1=$(reqs); hit=$((n1-n0))
[ "$code" = "200" ] && [ "$hit" = "2" ] && log "PASS 429-free -> 2 upstream hits (w1 banned, w2 ok), final 200" \
  || log "FAIL 429-free: code=$code hits=$hit"

# ---------- retry: generic 429 -> cooldown, next worker ----------
n0=$(reqs)
out=$(curl -s -w "|%{http_code}" -X POST http://127.0.0.1:$PORT/v1/chat/completions \
  -H 'Content-Type: application/json' -d '{"model":"m1","messages":[],"x-trigger-429":1}')
code=${out##*|}
n1=$(reqs); hit=$((n1-n0))
[ "$code" = "200" ] && [ "$hit" = "2" ] && log "PASS generic 429 -> rotate, final 200 (hits=$hit)" \
  || log "FAIL 429: code=$code hits=$hit"

# ---------- retry: 5xx -> rotate ----------
n0=$(reqs)
out=$(curl -s -w "|%{http_code}" -X POST http://127.0.0.1:$PORT/v1/chat/completions \
  -H 'Content-Type: application/json' -d '{"model":"m1","messages":[],"x-trigger-500":1}')
code=${out##*|}
n1=$(reqs); hit=$((n1-n0))
[ "$code" = "200" ] && [ "$hit" = "2" ] && log "PASS 5xx -> rotate, final 200 (hits=$hit)" \
  || log "FAIL 5xx: code=$code hits=$hit"

# ---------- retry: 4xx direct return, no rotation ----------
n0=$(reqs)
out=$(curl -s -w "|%{http_code}" -X POST http://127.0.0.1:$PORT/v1/chat/completions \
  -H 'Content-Type: application/json' -d '{"model":"m1","messages":[],"x-trigger-400":1}')
code=${out##*|}
n1=$(reqs); hit=$((n1-n0))
[ "$code" = "400" ] && [ "$hit" = "1" ] && log "PASS 4xx direct return, 1 hit only" \
  || log "FAIL 4xx: code=$code hits=$hit"

# ---------- retry: all workers 5xx -> last error surfaced ----------
n0=$(reqs)
out=$(curl -s -w "|%{http_code}" -X POST http://127.0.0.1:$PORT/v1/chat/completions \
  -H 'Content-Type: application/json' -d '{"model":"m1","messages":[],"x-trigger-500-all":1}')
code=${out##*|}
n1=$(reqs); hit=$((n1-n0))
[ "$code" = "500" ] && [ "$hit" = "3" ] && log "PASS all-fail -> 3 hits, last error surfaced (code 500)" \
  || log "FAIL all-fail: code=$code hits=$hit"

stop
