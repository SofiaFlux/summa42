#!/bin/sh
set -eu

if [ "$#" -ne 8 ] || [ "$1" != "exec" ] || [ "$2" != "--ephemeral" ] || [ "$3" != "--json" ] || [ "$4" != "--sandbox" ] || [ "$5" != "workspace-write" ] || [ "$6" != "--cd" ] || [ "$8" != "-" ]; then
  echo "unexpected argv: $*" >&2
  exit 41
fi
workspace="$7"
prompt="$(cat)"
printf '%s' "$prompt" > "$workspace/.fake_codex_prompt"

if [ -n "${GITHUB_TOKEN:-}" ] || [ -n "${AZURE_CLIENT_SECRET:-}" ] || [ -n "${SUMMA42_OWNER_PRIVATE_KEY:-}" ]; then
  echo "protected credential leaked into Codex environment" >&2
  exit 42
fi

printf '%s\n' '{"type":"thread.started","thread_id":"thread-test-123"}'
printf '%s\n' '{"type":"item.completed","item":{"id":"cmd-1","type":"command_execution","command":"printf ok","aggregated_output":"ok\\n","exit_code":0,"status":"completed"}}'
printf '%s\n' '{"type":"item.completed","item":{"id":"file-1","type":"file_change","changes":[{"path":"answer.txt","kind":"add"}],"status":"completed"}}'
printf '%s\n' '{"type":"item.completed","item":{"id":"msg-1","type":"agent_message","text":"Finished safely."}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":10,"cache_write_input_tokens":2,"output_tokens":20,"reasoning_output_tokens":5}}'
