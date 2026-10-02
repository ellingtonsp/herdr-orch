#!/bin/sh
# Fails when a private term appears in the tracked tree or in $PR_TEXT (a PR's title and body).
# The terms come from $PRIVATE_TERMS, one extended regex per line, matched case-insensitively.
# In CI they come from a repository secret, so this public repo never lists them. Output names
# only the file and line, never the matching text, because Actions logs on public repos are public.
set -eu

if [ -z "${PRIVATE_TERMS:-}" ]; then
  echo "private-terms: PRIVATE_TERMS is not set; nothing to check"
  exit 0
fi
pat=$(printf '%s\n' "$PRIVATE_TERMS" | sed '/^[[:space:]]*$/d' | paste -sd'|' -)
fail=0

hits=$(git grep -n -I -i -E "$pat" -- . ':!LICENSE' ':!go.sum' | cut -d: -f1,2 || true)
if [ -n "$hits" ]; then
  echo "private-terms: private content found at:"
  printf '%s\n' "$hits" | sed 's/^/  /'
  fail=1
fi

if [ -n "${PR_TEXT:-}" ] && printf '%s\n' "$PR_TEXT" | grep -q -i -E "$pat"; then
  echo "private-terms: the pull request title or description contains private content"
  fail=1
fi

[ $fail -eq 0 ] && echo "private-terms: clean"
exit $fail
