# svc-mcp: wraps a stdio MCP server as an HTTP MCP endpoint. A base image: a
# derived image bakes the wrapped server in and pins it (so the fleet.lock
# pin covers the software), e.g. svc-mcp-postgres below.
FROM node:24-bookworm-slim
COPY services/svc-mcp/server.mjs /opt/svc-mcp/server.mjs
COPY services/svc-mcp/service.json /a12s/service.json
USER 1000:1000
CMD ["node", "/opt/svc-mcp/server.mjs"]
