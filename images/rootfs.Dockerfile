# Guest root filesystem for the "python" template. Exported to a tarball and
# turned into a read-only ext4 image; kiln-agent overlays it at boot.
FROM python:3.12-alpine3.22
RUN apk add --no-cache bash git coreutils curl ca-certificates \
 && mkdir -p /kiln /mnt/kiln /workspace /run/kiln
COPY bin/kiln-agent /kiln/agent
