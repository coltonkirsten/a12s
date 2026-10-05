# a12s browser tier: slim + Debian Chromium + fonts + Playwright MCP.
# Build context: repo root.  docker build -f images/browser/Dockerfile -t a12s/browser:dev .
# Run with --security-opt seccomp=images/browser/seccomp.json so Chromium keeps its own sandbox.
ARG SLIM_IMAGE=a12s/slim:dev
FROM ${SLIM_IMAGE}

RUN apt-get update \
 && apt-get install -y --no-install-recommends chromium fonts-liberation fonts-noto-color-emoji \
 && rm -rf /var/lib/apt/lists/*

# Pinned by package-lock.json; uses Debian's Chromium, never downloads a browser.
ENV PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
COPY images/browser/playwright-mcp/ /opt/a12s/playwright-mcp/
RUN cd /opt/a12s/playwright-mcp \
 && npm ci --omit=dev \
 && npm cache clean --force \
 && rm -rf /root/.npm

# Lets agent scripts `require("playwright")`; launch with executablePath: "/usr/bin/chromium".
ENV NODE_PATH=/opt/a12s/playwright-mcp/node_modules \
    A12S_CHROMIUM=/usr/bin/chromium
