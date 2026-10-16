Vendored proto-generated Go bindings for substrate's Control gRPC API.

Source: `github.com/agent-substrate/substrate/pkg/proto/ateapipb` (Apache 2.0).
Pinned release: **v0.5.0**, commit
`e8d40c33f96f4b7a4984bfea7334a36ab5b45b7b`.
The source schema is `pkg/proto/ateapipb/ateapi.proto` in that release.

We copy the generated files here rather than `go get` the substrate module because
substrate brings ~hundreds of MB of cloud-provider deps that this proxy doesn't
need. When the upstream substrate proto changes (new RPCs, new fields), re-copy
`ateapi.pb.go` and `ateapi_grpc.pb.go` from the same pinned release, update
`test/versions.env`, and adapt the lifecycle/routing integration. Do not edit
the generated files manually.
