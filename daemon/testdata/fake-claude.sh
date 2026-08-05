#!/usr/bin/env bash
# Fake Claude Code for CI: ignores all flags, makes a deterministic config edit
# in the CWD (the job worktree), and emits the `claude -p --output-format json`
# result shape with a matching plan. Lets the whole pipeline run without a token.
mkdir -p config
printf 'message = "hi from jarjar test"\n' > config/test.toml
cat <<'JSON'
{"result":"done","num_turns":2,"stop_reason":"end_turn","total_cost_usd":0.01,
"structured_output":{"schema_version":1,"status":"ok","actions":[
{"type":"config_edit","path":"config/test.toml","description":"Set a test message"}],
"summary":"Added a test config value.","admin_notes":"ci","confidence":"high","clarification_question":null}}
JSON
