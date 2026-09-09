#!/usr/bin/env bash
# Verify, and optionally rewrite, the image digests in docker-compose.yml.
#
# Each image line must carry both a tag and a digest, per SDD section 11.1.
# This script resolves the current digest of each tag from the registry and
# compares it with the pinned value.
#
#   ./pin-images.sh          Report a mismatch and exit 1.
#   ./pin-images.sh --check  The same as no argument.
#   ./pin-images.sh --write  Rewrite each mismatched digest in place.
set -euo pipefail

cd "$(dirname "$0")"
COMPOSE_FILE="docker-compose.yml"
WRITE=0
case "${1:-}" in
  "" | --check) WRITE=0 ;;
  --write) WRITE=1 ;;
  # A silent fall back to check mode hides a mistyped --write, and the operator
  # then believes the file holds a digest that the script never wrote.
  *) echo "unknown argument: $1 (use --check or --write)" >&2; exit 2 ;;
esac

status=0

# Collect every "image: repo:tag@sha256:..." reference.
refs=$(grep -oE 'image: [^[:space:]]+@sha256:[0-9a-f]+' "$COMPOSE_FILE" | sed 's/^image: //' | sort -u)

if [ -z "$refs" ]; then
  echo "no digest-pinned image found in $COMPOSE_FILE" >&2
  exit 1
fi

for ref in $refs; do
  tagged="${ref%@*}"
  pinned="${ref#*@}"

  # awk reads the full stream. An early awk exit closes the pipe, inspect dies of
  # SIGPIPE on a long multi-architecture listing, and pipefail with set -e then
  # stops the whole script at that image. The || true keeps an unresolvable image
  # on the FAIL branch below instead of killing the run.
  actual=$(docker buildx imagetools inspect "$tagged" 2>/dev/null | awk '/^Digest:/ && !seen {print $2; seen=1}' || true)
  if [ -z "$actual" ]; then
    echo "FAIL  $tagged  cannot resolve digest from the registry"
    status=1
    continue
  fi

  if [ "$actual" = "$pinned" ]; then
    echo "OK    $tagged"
    continue
  fi

  if [ "$WRITE" -eq 1 ]; then
    # The digest is hex, so a plain substitution is safe.
    sed -i.bak "s|${tagged}@${pinned}|${tagged}@${actual}|g" "$COMPOSE_FILE"
    rm -f "${COMPOSE_FILE}.bak"
    echo "WROTE $tagged  $pinned -> $actual"
  else
    echo "STALE $tagged"
    echo "        pinned: $pinned"
    echo "        actual: $actual"
    status=1
  fi
done

exit $status
