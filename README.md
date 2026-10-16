# playwright-k8s-sandbox

A Kubernetes proxy that fronts Playwright (MCP HTTP + native WebSocket) and
routes each calling agent pod to its own Playwright sandbox. Five backends:
`agent-sandbox`, `openshell`, `substrate`, `karssandbox`, `isola`.

- Code: [`cmd/`](cmd/) and [`internal/`](internal/)
- How it works, sequence diagrams, bench results: **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)**
- Visual comparison of the four upgraded backends: **[Architecture comparison](docs/comparison.html)** (standalone HTML; open locally in a browser)
- Local kind harness and bench script: [`test/`](test/)
- Backend upgrades from the previous repo setup and kind rerun: **[October 2026 update](docs/blog/sandbox-updates-2026-10.md)**
- Deployment: **[DEPLOYMENT.md](DEPLOYMENT.md)** and [`deploy/`](deploy/)
