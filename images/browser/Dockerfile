# Railway builds this from the private repo (no registry credentials needed):
# Nix builds the runtime closure, which is copied into an empty image.
FROM nixos/nix:2.32.8 AS build
WORKDIR /src
COPY . .
# desktop-smoke first: it runs the desktop_execute tool against a real Xvnc,
# so an image whose nut.js cannot drive the display is never built.
# exec-smoke does the same for shell commands (mcp-exec, as the entrypoint
# starts it), and checks that a web page's requests to it are refused.
RUN nix --extra-experimental-features "nix-command flakes" --option filter-syscalls false \
      build -L --no-link .#desktop-smoke \
 && nix --extra-experimental-features "nix-command flakes" --option filter-syscalls false \
      build -L --no-link .#exec-smoke \
 && nix --extra-experimental-features "nix-command flakes" --option filter-syscalls false \
      build .#runtime -o /result \
 && mkdir -p /rootfs/nix/store /rootfs/tmp /rootfs/root /rootfs/etc /rootfs/data \
 && cp -a $(nix-store -qR /result) /rootfs/nix/store/ \
 && ln -s "$(readlink -f /result)/bin/browser-entrypoint" /rootfs/entrypoint \
 && mkdir -p /rootfs/bin /rootfs/usr/bin \
 && ln -s "$(readlink -f /result/bin/browser-entrypoint | xargs grep -o '/nix/store/[^/]*-bash-[^/]*/bin/bash' | head -1)" /rootfs/bin/sh \
 && printf 'root:x:0:0:root:/root:/bin/sh\nbrowser:x:1000:1000:browser:/home/browser:/bin/sh\n' > /rootfs/etc/passwd \
 && printf 'root:x:0:\nbrowser:x:1000:\n' > /rootfs/etc/group \
 && mkdir -p /rootfs/home/browser \
 && chown 1000:1000 /rootfs/home/browser \
 && chmod 700 /rootfs/home/browser \
 && chmod 1777 /rootfs/tmp

# The image runs as root unless told otherwise, which is what the standalone
# (Railway) deployment does: its volume at /data belongs to root. A browserjs
# session pod runs it as the user "browser" (runAsUser: 1000), which the
# entrypoint supports equally; openbox needs that uid to be in /etc/passwd.
FROM scratch
COPY --from=build /rootfs /
EXPOSE 8080 8081
ENTRYPOINT ["/entrypoint"]
