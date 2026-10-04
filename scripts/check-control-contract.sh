#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH='' cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd "$SCRIPT_DIR/.." && pwd)
CONTRACT="$REPO_ROOT/docs/stack.md"

APPROVED_IDS='CTRL-CONS-01
CTRL-S3-01
CTRL-IDEM-01
CTRL-WEB-01
CTRL-RATE-01
CTRL-MEDIA-01
CTRL-TRACE-01
CTRL-EVID-01
CTRL-VERS-01
CTRL-RESTORE-01
CTRL-READY-01
CTRL-SHUTDOWN-01'

FORMS='.github/ISSUE_TEMPLATE/user-story.yml
.github/ISSUE_TEMPLATE/technical-enabler.yml
.github/ISSUE_TEMPLATE/pilot-validation.yml'

fail() {
  printf 'ERROR: %s\n' "$1" >&2
  exit 1
}

[ -f "$CONTRACT" ] || fail 'falta docs/stack.md'

start_count=$(grep -c '^<!-- CONTROL-CONTRACT:START -->$' "$CONTRACT" || true)
end_count=$(grep -c '^<!-- CONTROL-CONTRACT:END -->$' "$CONTRACT" || true)
if [ "$start_count" -ne 1 ] || [ "$end_count" -ne 1 ]; then
  fail 'bloque canónico inválido'
fi

start_line=$(awk '/^<!-- CONTROL-CONTRACT:START -->$/ { print NR }' "$CONTRACT")
end_line=$(awk '/^<!-- CONTROL-CONTRACT:END -->$/ { print NR }' "$CONTRACT")
[ "$start_line" -lt "$end_line" ] || fail 'orden de marcadores inválido'

approved_count=$(printf '%s\n' "$APPROVED_IDS" | grep -c '^CTRL-' || true)
[ "$approved_count" -eq 12 ] || fail 'la lista aprobada debe contener 12 IDs'

canonical_block=$(sed -n '/^<!-- CONTROL-CONTRACT:START -->$/,/^<!-- CONTROL-CONTRACT:END -->$/p' "$CONTRACT")
found_ids=$(printf '%s\n' "$canonical_block" | tr -cs 'A-Z0-9-' '\n' | grep '^CTRL-' || true)
control_headings=$(printf '%s\n' "$canonical_block" | sed -n 's/^### \(CTRL-[A-Z0-9][A-Z0-9-]*\) — .*/\1/p')

printf '%s\n' "$APPROVED_IDS" | while IFS= read -r control_id; do
  count=$(printf '%s\n' "$control_headings" | grep -cx "$control_id" || true)
  [ "$count" -eq 1 ] || fail "$control_id debe ser un encabezado canónico único"
done

printf '%s\n' "$found_ids" | while IFS= read -r control_id; do
  [ -n "$control_id" ] || continue
  printf '%s\n' "$APPROVED_IDS" | grep -qx "$control_id" || fail "ID desconocido en el bloque canónico: $control_id"
done

printf '%s\n' "$FORMS" | while IFS= read -r relative_form; do
  form="$REPO_ROOT/$relative_form"
  [ -f "$form" ] || fail "falta $relative_form"

  field_count=$(grep -c '^[[:space:]]*id: cross_cutting_controls$' "$form" || true)
  [ "$field_count" -eq 1 ] || fail "$relative_form debe tener un único cross_cutting_controls"

  awk '
    /^  - type:/ { current_type = $3 }
    /^[[:space:]]*id: cross_cutting_controls$/ {
      found = 1
      valid = current_type == "textarea"
    }
    END { exit !(found && valid) }
  ' "$form" || fail "cross_cutting_controls debe ser textarea en $relative_form"

  awk '
    /^  - type:/ {
      if (in_target) {
        exit found_required ? 0 : 1
      }
    }
    /^[[:space:]]*id: cross_cutting_controls$/ { in_target = 1 }
    in_target && /^[[:space:]]*required: true$/ { found_required = 1 }
    END {
      if (in_target) {
        exit found_required ? 0 : 1
      }
      exit 1
    }
  ' "$form" || fail "cross_cutting_controls no es requerido en $relative_form"

  grep -q 'docs/stack\.md' "$form" || fail "$relative_form no referencia docs/stack.md"

  awk '
    /^[[:space:]]*id: cross_cutting_controls$/ { controls_line = NR }
    /^[[:space:]]*id: definition_of_done$/ { done_line = NR }
    END { exit !(controls_line && done_line && controls_line < done_line) }
  ' "$form" || fail "cross_cutting_controls debe preceder definition_of_done en $relative_form"
done

printf 'OK: contrato de controles válido\n'
