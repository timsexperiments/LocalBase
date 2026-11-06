# A disposable Debian with systemd as PID 1, for tools/systemd-check.sh.
# The base is pinned by digest (debian:bookworm-slim index, 2026-10-21).
FROM debian@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
ENV DEBIAN_FRONTEND=noninteractive container=docker
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus dbus-user-session libpam-systemd curl ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 1000 --shell /bin/bash lb
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
