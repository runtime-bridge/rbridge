# Performance

`make bench` measures what rbridge adds on top of a runtime. Numbers from a laptop (Intel, Linux, Go 1.26), 2026-11-25:

| Benchmark | Result |
|---|---|
| Event handling per event (`BenchmarkSequencer`) | 39 ns, 0 allocations |
| Policy decision (`BenchmarkPolicyDecide`) | 1.1 µs, 13 allocations |
| Skill digest, 50 files of 4 KiB (`BenchmarkDigest`) | 0.8 ms |
| Start an ACP agent, handshake, one turn, close (`BenchmarkOpenAndTurn`, fake agent) | 4.2 ms |

Real runtimes take seconds to start and answer, so rbridge's share is small. `TestAllocationBudget` runs with the normal tests and fails if the event path or policy decisions start allocating more than their budgets.
