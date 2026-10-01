# Railway builds this from the private repo (no registry credentials needed):
# Nix builds the runtime closure, which is copied into an empty image.
FROM nixos/nix:2.32.8 AS build
WORKDIR /src
COPY . .
RUN nix --extra-experimental-features "nix-command flakes" --option filter-syscalls false \
      build .#runtime -o /result \
 && mkdir -p /rootfs/nix/store /rootfs/tmp /rootfs/root /rootfs/etc /rootfs/data \
 && cp -a $(nix-store -qR /result) /rootfs/nix/store/ \
 && ln -s "$(readlink -f /result)/bin/browser-entrypoint" /rootfs/entrypoint \
 && mkdir -p /rootfs/bin /rootfs/usr/bin \
 && ln -s "$(readlink -f /result/bin/browser-entrypoint | xargs grep -o '/nix/store/[^/]*-bash-[^/]*/bin/bash' | head -1)" /rootfs/bin/sh \
 && printf 'root:x:0:0:root:/root:/bin/sh\n' > /rootfs/etc/passwd \
 && printf 'root:x:0:\n' > /rootfs/etc/group \
 && chmod 1777 /rootfs/tmp

FROM scratch
COPY --from=build /rootfs /
EXPOSE 8080 8081
ENTRYPOINT ["/entrypoint"]
