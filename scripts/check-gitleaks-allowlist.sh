#!/bin/sh
# Every allowlist entry in .gitleaks.toml is one exact value, with a one-line reason above it.
# A dot is written \\. so that nothing in an entry acts as a pattern.
set -eu
awk '
  /^ *\047\047\047/ {
    entries++
    value = $0
    sub(/^ *\047\047\047/, "", value)
    sub(/\047\047\047,? *$/, "", value)
    if (previous !~ /^ *# *[^ ]/) { print "no reason above: " value; bad = 1 }
    plain = value
    gsub(/\\\./, "", plain)
    if (plain !~ /^[A-Za-z0-9_=-]+$/) { print "not one exact value: " value; bad = 1 }
  }
  { previous = $0 }
  END {
    if (!entries) { print "no allowlist entries found"; bad = 1 }
    exit bad
  }
' "${1:-.gitleaks.toml}"
