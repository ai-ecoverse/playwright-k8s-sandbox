# Tiny Node HTTP server used as a CONTROL workload for substrate gVisor tests.
# Pure Node + a single-file server, no browser. If THIS doesn't reach
# STATUS_RUNNING under substrate's golden-actor workflow, the failure isn't
# Chromium-specific.
FROM node:23-bookworm-slim@sha256:86191b94d2a163be41f3dc7fe5e5fcaca8ba2f1be7275d98a06343483c17414a

WORKDIR /app
COPY server.js /app/server.js

EXPOSE 9222
CMD ["node", "/app/server.js"]
