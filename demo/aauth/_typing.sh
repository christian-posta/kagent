# Shared command display for aauth demo scripts (source only, do not execute).
#
# TYPE_DELAY — base scale for per-character pauses (default 0.028).
# FAST=1     — print everything instantly, no animation.

_char_sleep() {
  local c="$1"
  awk -v c="$c" -v r="$RANDOM" -v scale="${TYPE_DELAY:-0.028}" 'BEGIN {
    if (c == " ")
      { lo = 0.9; hi = 2.4 }
    else if (c ~ /[;|&]/)
      { lo = 1.6; hi = 3.8 }
    else if (c ~ /[.,:)]/)
      { lo = 1.1; hi = 2.5 }
    else
      { lo = 0.45; hi = 1.15 }
    printf "%.4f", scale * (lo + (r / 32767) * (hi - lo))
  }'
}

type_command() {
  local cmd="$1"
  if [[ -n "${FAST:-}" ]]; then
    printf '\n%s %s\n' "${PROMPT:-\$}" "$cmd"
    return
  fi

  # Multi-line: show the full block at once (still prefixed with the prompt).
  if [[ "$cmd" == *$'\n'* ]]; then
    printf '\n%s\n%s\n' "${PROMPT:-\$}" "$cmd"
    return
  fi

  # Single line: type with human-ish variable speed.
  printf '\n%s ' "${PROMPT:-\$}"
  local i c delay
  for ((i = 0; i < ${#cmd}; i++)); do
    c="${cmd:i:1}"
    printf '%s' "$c"
    delay="$(_char_sleep "$c")"
    sleep "$delay"
  done
  printf '\n'
}
