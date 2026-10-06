#!/bin/sh
set -eu

enforce=false
if [ "${1:-}" = "--enforce" ]; then
  enforce=true
fi

python_files="$(rg --files -g '*.py' | wc -l | tr -d ' ')"
legacy_calls="$(rg -n 'postLegacy\(' backend/internal/httpserver -g '*.go' 2>/dev/null | wc -l | tr -d ' ')"
legacy_proxy_refs="$(rg -n 'legacyProxy|LegacyBackendURL|NASTOOL_LEGACY' backend docker Makefile 2>/dev/null | wc -l | tr -d ' ')"
python_runtime_refs="$(rg -n 'python|pip install|requirements\.txt|run\.py|svc-nastools(/|$)' docker Makefile 2>/dev/null | wc -l | tr -d ' ')"

printf 'Python source files: %s\n' "$python_files"
printf 'Go legacy endpoint calls: %s\n' "$legacy_calls"
printf 'Legacy proxy/runtime references: %s\n' "$legacy_proxy_refs"
printf 'Container Python runtime references: %s\n' "$python_runtime_refs"

if [ "$enforce" = true ] && [ "$((python_files + legacy_calls + legacy_proxy_refs + python_runtime_refs))" -ne 0 ]; then
  printf 'Python exit gate: FAILED\n' >&2
  exit 1
fi

if [ "$((python_files + legacy_calls + legacy_proxy_refs + python_runtime_refs))" -eq 0 ]; then
  printf 'Python exit gate: PASSED\n'
else
  printf 'Python exit gate: migration in progress\n'
fi
