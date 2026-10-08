# svc-mcp: wraps a stdio MCP server as an HTTP MCP endpoint. A base image: a
# derived image bakes the wrapped server in and pins it (fleet.lock pins the
# software, enhancements §5.6), e.g. svc-mcp-postgres below.
FROM node:24-bookworm-slim
COPY services/svc-mcp/server.mjs /opt/svc-mcp/server.mjs
COPY services/svc-mcp/service.json /a12s/service.json
USER 1000:1000
CMD ["node", "/opt/svc-mcp/server.mjs"]
