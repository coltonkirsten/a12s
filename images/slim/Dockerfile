# a12s slim tier: Debian slim + Node + basic tools + computerd. Never contains a harness.
# Build context: repo root.  docker build -f images/slim/Dockerfile -t a12s/slim:dev .

# computerd is plain JavaScript, so it builds once on the build platform for every target.
FROM --platform=$BUILDPLATFORM node:24-bookworm-slim AS computerd
WORKDIR /src
COPY computerd/package.json computerd/package-lock.json ./
# The SDK's optional bundled claude binary is never used (the HarnessBundle supplies it).
RUN npm ci --omit=optional
COPY computerd/ ./
RUN npm run build \
 && npm prune --omit=dev --omit=optional \
 && mkdir -p node_modules \
 && test -f dist/main.js

FROM node:24-bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git ripgrep python3 curl util-linux \
 && rm -rf /var/lib/apt/lists/*

# The base image's `node` user owns uid/gid 1000; replace it with `agent`.
# No home directory is created: the home drive is mounted over /home at runtime
# and computerd creates /home/agent there.
RUN userdel node \
 && rm -rf /home/node \
 && groupadd -g 1000 agent \
 && useradd -u 1000 -g 1000 -M -d /home/agent -s /bin/bash agent

COPY --from=computerd /src/package.json /opt/a12s/computerd/package.json
COPY --from=computerd /src/node_modules /opt/a12s/computerd/node_modules
COPY --from=computerd /src/dist /opt/a12s/computerd/dist
RUN mkdir -p /opt/a12s/bundles

# Custom Computer images are built FROM this one and inherit the label; admission refuses an image without it.
ARG A12S_VERSION=dev
LABEL ai.a12s.computerd.version=$A12S_VERSION

ENV NODE_ENV=production
# computerd runs as root (with a reduced capability set from the driver) and
# spawns the harness as uid/gid 1000; hence no USER here.
ENTRYPOINT ["node", "/opt/a12s/computerd/dist/main.js"]
