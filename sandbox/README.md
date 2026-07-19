# sandbox

Production-grade software built on **databox** as its backend, developed and
maintained here. Each project in this directory is real software — the `examples/`
name is gone deliberately — and is expected to graduate one day into its own
repository. Until then it lives in-tree so it evolves lockstep with databox and
exercises the store under real workloads.

Each subproject is its own Go module with a `replace github.com/hyperkubeorg/databox
=> ../../` directive, so it builds against the databox source in this repo.

| Project | Description |
| --- | --- |
| [personalcloudplatform](personalcloudplatform/README.md) | PCP — a self-hosted personal cloud platform (Drive, Email, Calendar, Contacts, Music, Video, Messenger, Git Services, Builds) with blind mail relays and web gateways. |
