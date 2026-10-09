# Tiny Node HTTP server used as a CONTROL workload for substrate gVisor tests.
# Pure Node + a single-file server, no browser. If THIS doesn't reach
# STATUS_RUNNING under substrate's golden-actor workflow, the failure isn't
# Chromium-specific.
FROM node:26-bookworm-slim@sha256:86f07bc9c5dce4578cf37e5a418b7bfc7f817cda25cde66e2b66e95ed86c4567

WORKDIR /app
COPY server.js /app/server.js

EXPOSE 9222
CMD ["node", "/app/server.js"]
