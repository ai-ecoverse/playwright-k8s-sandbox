# Tiny Node HTTP server used as a CONTROL workload for substrate gVisor tests.
# Pure Node + a single-file server, no browser. If THIS doesn't reach
# STATUS_RUNNING under substrate's golden-actor workflow, the failure isn't
# Chromium-specific.
FROM node:26-bookworm-slim@sha256:cd9f682fa2885cd1056e830424764158570061c59736a1da836bc3d73df095ae

WORKDIR /app
COPY server.js /app/server.js

EXPOSE 9222
CMD ["node", "/app/server.js"]
