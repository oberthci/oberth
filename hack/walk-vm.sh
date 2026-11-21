#!/usr/bin/env bash
set -Eeuo pipefail
work=${WALK_SHARED:-/work/shared}
key_dir=$work/walk-key
key=$key_dir/id_ed25519
known=$key_dir/known_hosts
trap 'rm -f "$key" "$key.pub" "$known"' EXIT
: "${WALK_VM_ADDRESS:?Provide the admitted VM address}"
[[ -f $key && -f $work/dist/oberth.tgz ]] || { echo 'Candidate bytes or VM key are missing.' >&2; exit 1; }
common=(-o "UserKnownHostsFile=$known" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5)
for attempt in $(seq 1 60); do
    if ssh "${common[@]}" -o BatchMode=yes -o PasswordAuthentication=no -i "$key" "walk@$WALK_VM_ADDRESS" true; then break; fi
    [[ $attempt != 60 ]] || exit 1
    sleep 2
done
remote=(ssh "${common[@]}" -o BatchMode=yes -o PasswordAuthentication=no -i "$key" "walk@$WALK_VM_ADDRESS")
"${remote[@]}" 'mkdir -p "$HOME/candidate/src" "$HOME/evidence"'
tar -cf - --exclude=.git . | "${remote[@]}" 'tar -xf - -C "$HOME/candidate/src"'
tar -cf - -C "$work" dist | "${remote[@]}" 'tar -xf - -C "$HOME/candidate"'
status=0
"${remote[@]}" '"$HOME/candidate/src/test/walk/walk" --mode fresh --counts-only --candidate "$HOME/candidate/dist" --env /etc/oberth-walk/environment.json --out "$HOME/evidence/result"' || status=$?
"${remote[@]}" 'tar -cf - -C "$HOME/evidence" result' | tar -xf - -C "$work"
exit "$status"
