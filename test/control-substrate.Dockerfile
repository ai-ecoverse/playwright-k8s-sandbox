# Tiny Node HTTP server used as a CONTROL workload for substrate gVisor tests.
# Pure Node + a single-file server, no browser. If THIS doesn't reach
# STATUS_RUNNING under substrate's golden-actor workflow, the failure isn't
# Chromium-specific.
FROM node:26-bookworm-slim@sha256:28147155d47dc31abe0a46e0d12b59b2fb3d0a2dcc5dc65fce76a074180b16fe

WORKDIR /app
COPY server.js /app/server.js

EXPOSE 9222
CMD ["node", "/app/server.js"]
