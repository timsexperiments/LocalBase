# A disposable Debian with systemd as PID 1, for tools/systemd-check.sh.
# The base is pinned by digest (debian:bookworm-slim index, 2026-10-21).
FROM debian@sha256:9c6e381421d0f48d1b8133e573f33c185ee16cbf505eda9c4a8a4791b25ea8a6
ENV DEBIAN_FRONTEND=noninteractive container=docker
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus dbus-user-session libpam-systemd curl ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 1000 --shell /bin/bash lb
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
