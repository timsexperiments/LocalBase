#!/bin/bash
# What macOS does as free memory falls, for choosing the memory reserve
# (docs/memory-reserve.md). For a real 8 to 24 GiB Mac. Nothing here changes
# settings or files.
#
#   scripts/reserve-check.sh sample [seconds]   read-only; safe with anything running
#   scripts/reserve-check.sh probe  [floorGiB]  allocates memory; see the limits below
#
# probe refuses to start if LocalBase or a model runtime is running, or on more
# than 24 GiB. It takes 256 MiB at a time, and stops as soon as any of these holds:
#   - free memory would fall below the floor (default 4 GiB, above Option C's 3.75);
#   - macOS reports any pressure above normal;
#   - it holds half the machine's memory;
#   - 5 minutes have passed (a hard alarm that ends the process).
# It then holds for 30 seconds while sampling, and exits, which frees everything.
# Save your work and close what you can first. Ctrl-C stops it at once.
set -u

sample_line() {
  vm_stat | awk -v t="$(date +%H:%M:%S)" -v lvl="$(sysctl -n kern.memorystatus_vm_pressure_level)" -v pct="$(sysctl -n kern.memorystatus_level)" '
    /page size of/{for(i=1;i<=NF;i++) if($i=="of") ps=$(i+1)}
    /Pages free/{gsub("\\.","",$3);f=$3} /Pages speculative/{gsub("\\.","",$3);s=$3} /Pages inactive/{gsub("\\.","",$3);n=$3}
    /wired down/{gsub("\\.","",$4);w=$4} /occupied by compressor/{gsub("\\.","",$5);c=$5} /Swapouts/{gsub("\\.","",$2);o=$2}
    END{g=ps/1073741824; printf "%s free=%.1f wired=%.1f compressor=%.1f swapouts=%s level=%s memfree%%=%s\n",t,(f+s+n)*g,w*g,c*g,o,lvl,pct}'
}

case "${1:-}" in
sample)
  end=$((SECONDS + ${2:-60}))
  while [ "$SECONDS" -lt "$end" ]; do sample_line; sleep 5; done
  ;;
probe)
  floor=${2:-4}
  total=$(($(sysctl -n hw.memsize) / 1073741824))
  [ "$total" -le 24 ] || { echo "refusing: this check is for 8 to 24 GiB Macs; this one has $total GiB." >&2; exit 2; }
  if pgrep -lf 'local-base|localbase|llama-server|sd-server|whisper-server|tts-server' | grep -v reserve-check >/dev/null ||
    lsof -nP -iTCP:2273 -sTCP:LISTEN >/dev/null 2>&1; then
    echo "refusing: LocalBase or a model runtime is running. Stop it first." >&2
    exit 2
  fi
  exec perl -e '
    use strict; $| = 1;
    my ($floor, $total) = @ARGV;
    alarm 300; # the hard limit: SIGALRM ends this process, and with it the memory
    my $chunk = 256 * 1024 * 1024;
    sub state {
      my $v = `vm_stat`;
      my ($ps) = $v =~ /page size of (\d+)/;
      my $free = 0;
      for my $k ("Pages free", "Pages speculative", "Pages inactive") { $free += $1 if $v =~ /\Q$k\E:\s+(\d+)/; }
      my $lvl = `sysctl -n kern.memorystatus_vm_pressure_level`; chomp $lvl;
      my ($c) = $v =~ /occupied by compressor:\s+(\d+)/;
      return ($free * $ps / 2**30, $lvl, ($c // 0) * $ps / 2**30);
    }
    open(my $r, "<:raw", "/dev/urandom") or die "urandom: $!";
    my @held; my $why = "half the memory held";
    while (@held * 0.25 < $total / 2) {
      my ($free, $lvl, $comp) = state();
      printf "held=%.2f free=%.1f compressor=%.1f level=%s\n", @held * 0.25, $free, $comp, $lvl;
      if ($lvl != 1) { $why = "macOS reported pressure level $lvl"; last; }
      if ($free - 0.25 < $floor) { $why = "the $floor GiB floor"; last; }
      read($r, my $buf, $chunk) == $chunk or die "short read";
      push @held, $buf; # random data: the compressor cannot shrink it
    }
    print "stopped: $why. Holding 30 seconds; note how responsive the machine feels.\n";
    for (1 .. 6) { my ($f, $l, $c) = state(); printf "held=%.2f free=%.1f compressor=%.1f level=%s\n", @held * 0.25, $f, $c, $l; sleep 5; }
    print "done; exiting frees the memory.\n";
  ' "$floor" "$total"
  ;;
*)
  sed -n 2,16p "$0" | sed 's/^# \{0,1\}//'
  exit 2
  ;;
esac
