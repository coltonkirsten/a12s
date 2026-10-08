# svc-mcp-postgres: svc-mcp with @modelcontextprotocol/server-postgres baked in
# at a fixed version, so the pin pins the software (no npx at start).
ARG SVC_MCP_IMAGE=a12s/svc-mcp:0.1
FROM ${SVC_MCP_IMAGE}
USER root
RUN npm i -g --prefix /opt/mcp @modelcontextprotocol/server-postgres@0.6.2 \
 && npm cache clean --force && rm -rf /root/.npm
USER 1000:1000
