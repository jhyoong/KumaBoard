# GPU metrics — research note

Status: research only (no code changed).

Goal: have `kuma-agent` report GPU metrics next to its CPU/mem/disk sample and show them
on the dashboard. The limits are `CGO_ENABLED=0` everywhere, the shared `proto/`
package with its N/N-1 window, agents that run unprivileged on every OS, and a 30 s
`metrics_interval_s` tick (`configs/kumaboard.example.yaml`).

---

## 0. What the code does today (grounding)

| Area | Fact | Where |
|---|---|---|
| Wire shape | `proto.Metrics` is flat. Its fields are plain values in snake_case JSON with **no pointers and no `omitempty`**. A collector that fails leaves its field at zero. | `proto/messages.go` |
| Optional-in-JSON precedent | `Envelope.Payload` uses `omitempty`. The registry uses pointers for "may be absent" (`*proto.Metrics`, `*time.Time`). | `proto/envelope.go`, `server/registry/registry.go` |
| Version gate | `Version = 1`, `MinSupported = 1`, `Supported(v) = MinSupported <= v <= Version`. It is checked **only** at handshake (`hub.go:125`). `Envelope.V` is not checked per message. | `proto/version.go`, `server/hub/hub.go` |
| Decoding strictness | The hub decodes agent payloads with plain `json.Unmarshal`. That call **ignores unknown fields**. `DisallowUnknownFields` is used only for dashboard request bodies. | `server/hub/hub.go:181`, `server/api/json.go:20` |
| Collector pattern | `collectors.Collect(ctx)` is a stateless package function built on gopsutil v4. Each collector fails softly, and an error comes back only if every collector fails. The only platform split is `root_unix.go` (`!windows`) and `root_windows.go`, which hold `rootPath` and `errNoCollectors`. | `agent/collectors/` |
| Metrics loop | This runs only if the `metrics` capability is set (`a.has(proto.CapMetrics)`). It uses one `Collect` per tick. | `agent/app/app.go:117,245` |
| Selftest | The pre-upgrade selftest calls `collectors.Collect` and **fails the upgrade** if it errors. | `cmd/kuma-agent/main.go:~99` |
| Capabilities | These are free-form strings in the agent YAML with no validation. They are sent in `hello` and stored in `devices.capabilities_json`. Constants live in `proto/messages.go`. | `agent/config/config.go`, `server/store/devices.go` |
| Server storage | Metrics are **not persisted in SQLite**. Each device keeps a 60-sample in-memory `Ring` of `proto.Metrics` values (`server/registry/ring.go`, `NewRing(60)`). The ring lives in `server/registry`, not `server/hub`. | `server/registry/` |
| SSE / API | The SSE event `metrics` is `{device, metrics: proto.Metrics}` (`MetricsEvent`). `GET /api/devices/{name}/metrics` returns `[]proto.Metrics`. `Device.metrics` is the latest sample. | `server/registry/registry.go`, `server/api/devices.go` |
| Web | The `Metrics` TS interface mirrors the Go struct. `DeviceCard` renders a 2-col text grid. `DeviceDetail` renders `Sparkline`s (0–100) from a 60-sample history. `state.ts` passes `metrics` through unchanged. | `web/src/api.ts`, `components/DeviceCard.tsx`, `views/DeviceDetail.tsx`, `components/Sparkline.tsx` |
| Service identities | Linux: systemd `User=kuma-agent`. macOS: LaunchDaemon `UserName=kuma-agent`. Windows: service logon `.\kuma-agent`, a local non-admin account (`cmd/kuma-agent/platform_windows.go` `-account` default). **No agent runs as root or admin.** | `deploy/` |
| Build targets | linux/amd64, linux/arm64 (pi), darwin/arm64, windows/amd64. | `Makefile` |
| Deps already in graph | `github.com/ebitengine/purego v0.10.2` (indirect, via gopsutil on darwin) and `golang.org/x/sys`. | `go.mod` |

Local probe of the control-plane host (Debian 13, kernel 6.12, Intel HD 530 / i915):
- `/sys/class/drm/card0/device/{vendor,device}` = `0x8086`/`0x1912`. `power/runtime_status` = `active`.
- `/sys/class/drm/card0/gt/gt0/rc6_residency_ms` and `rps_act_freq_mhz` are mode `0444` and readable unprivileged.
- `/sys/class/powercap/intel-rapl:*/energy_uj` is mode `0400 root`. This is the post-CVE-2020-8694 "Platypus" hardening, so RAPL power is unavailable to us.
- `/proc/sys/kernel/perf_event_paranoid` = `3`. Debian's patch blocks unprivileged `perf_event_open` entirely, so the i915 PMU and `intel_gpu_top` are unavailable without `CAP_PERFMON`.
- There is no hwmon for the iGPU (only `coretemp`), and `nvidia-smi` and `intel_gpu_top` are not installed.

---

## 1. Collection strategy per OS

No portable GPU API exists, and gopsutil v4 has no GPU package. cgo is ruled out.
That leaves three mechanisms:

| Mechanism | Cost per 30 s tick | Fragility | Security |
|---|---|---|---|
| Direct reads (sysfs files, IOKit via purego, PDH via `pdh.dll` syscalls) | microseconds, no process | a few stable kernel/OS ABIs | no exec surface |
| Shell out to vendor tool (`ioreg`, `nvidia-smi`, `powermetrics`) | 5–300 ms plus fork/exec; `nvidia-smi` can take ~1 s when persistence mode is off | text parsing that breaks across tool versions | exec of a PATH-resolved binary, so use an absolute path |
| cgo (NVML, IOKit, DXGI headers) | — | — | **forbidden** (`CGO_ENABLED=0`) |

**Principle:** use direct reads everywhere. Shell out only when no direct read exists
(NVIDIA temperature/power). Resolve the binary once, by absolute path, at probe time.

### 1a. macOS / Apple Silicon (macos-workstation, macos-desktop)

- **IORegistry `IOAccelerator` → `PerformanceStatistics`.** Every Apple GPU driver
  (`AGXAcceleratorG13X`, `G14X`, … subclasses of `IOAccelerator`) publishes a
  `PerformanceStatistics` dictionary. Reading it is plain unprivileged IOKit: no root, no
  entitlements, and it works from a LaunchDaemon running as `kuma-agent`. This is the same
  source that `gpuer`, `macmon`, `asitop`-style tools, and Activity Monitor-alikes use.
  - CLI equivalent, useful for verifying on the device:
    `ioreg -r -d 1 -c IOAccelerator` (add `-a` for plist XML). Example output shape
    (keys vary slightly by generation):
    ```
    "model" = "Apple M2 Max"
    "gpu-core-count" = 38
    "PerformanceStatistics" = {
        "Device Utilization %"=12, "Renderer Utilization %"=11, "Tiler Utilization %"=3,
        "In use system memory"=1234567890, "Alloc system memory"=2345678901,
        "recoveryCount"=0, "lastRecoveryTime"=0, "SplitSceneCount"=0, ... }
    ```
  - Fields we use: `Device Utilization %` → `util_percent`. `In use system memory` → `mem_used_bytes`,
    which is unified memory and therefore already counted in host mem. `model` → `name`.
    `gpu-core-count` is informational.
  - **No VRAM total** (unified memory), so `mem_total_bytes` stays absent. Do not synthesize it.
- **Transport:** gopsutil v4 already calls IOKit and CoreFoundation **without cgo** through
  `purego` (`internal/common/common_darwin.go`: `purego.Dlopen(IOKitLibPath)`,
  `IOServiceMatching`, `IOServiceGetMatchingServices`, `CFDictionaryGetValue`,
  `CFNumberGetValue`, …). Those helpers are `internal/`, so we cannot import them. We copy
  the pattern (~120 lines) and promote `purego` to a direct dependency. That gives zero
  spawns and cross-compiles from Linux with `CGO_ENABLED=0`, which is how
  `agent-darwin-arm64` is built today.
  The rejected alternative is to spawn `/usr/sbin/ioreg -a` every tick and parse the
  plist XML. That costs ~10–30 ms per tick and needs a plist decoder. It is simpler to
  write, but it adds a process per tick for no benefit once purego is in hand. We keep it
  as the manual verification command.
- **`powermetrics`** (GPU residency, GPU power mW, frequency) **requires root**
  (`sudo powermetrics --samplers gpu_power`). It is rejected: we would have to run the
  agent as root or add a sudoers rule, and it is a sampling tool that blocks for its sample
  interval.
- **Temperature and power:** SMC keys are reachable through `AppleSMC` IOKit user clients
  or IOHID sensor services without root. The key names are undocumented and change per SoC
  generation (M1 vs M2 vs M3/M4). This is deferred and not in v1.

### 1b. Windows (windows-desktop; windows-headless usually has no discrete GPU)

- **PDH "GPU Engine" and "GPU Adapter Memory" counters** (WDDM 2.x, Windows 10 1709+), the
  same data Task Manager uses:
  - `\GPU Engine(*)\Utilization Percentage`. Instances look like
    `pid_1234_luid_0x00000000_0x0000C9E1_phys_0_eng_0_engtype_3D`. This is a rate counter,
    so it needs two collections, and the **first tick yields nothing**.
  - `\GPU Adapter Memory(*)\Dedicated Usage` and `\Shared Usage` (bytes), one instance per
    adapter LUID.
  - Aggregation that matches Task Manager: group by `luid`, sum over pids per `engtype`,
    clamp each engine-type sum to 100, then take **the max over engine types**. That max is
    the adapter's `util_percent`.
- **Transport:** `pdh.dll` through `golang.org/x/sys/windows.NewLazySystemDLL`, calling
  `PdhOpenQuery`, `PdhAddEnglishCounterW`, `PdhCollectQueryData`, and
  `PdhGetFormattedCounterArrayW`. This is plain syscalls with no cgo, and gopsutil does the
  same in its internal `common_windows.go`, which again we cannot import. The query handle
  is **kept open across ticks**, which is why the collector must become stateful (see §3).
- **Privileges:** local PDH reads do not need admin. The "Performance Monitor Users" group
  only matters for *remote* counter access, and non-elevated Task Manager shows these same
  counters. Still, **verify on windows-desktop under the `.\kuma-agent` service account**, since
  services run in session 0. If per-process instances of other users' processes turn out
  to be hidden, utilization would read low. That would be a real defect, so the test should
  run a GPU load in the desktop session.
- **Adapter name and total VRAM:** read the registry at
  `HKLM\SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}\00NN`.
  `DriverDesc` gives the name and `HardwareInformation.qwMemorySize` (QWORD) gives the VRAM
  total. This key is readable by Users. Use `golang.org/x/sys/windows/registry`: pure Go
  with no spawn. Mapping LUID to registry key is not direct. For v1, attach name and total
  only when exactly one non-software adapter has non-zero dedicated usage. Otherwise report
  per-LUID entries with `name` = the LUID. Skip the "Microsoft Basic Render Driver".
- DXGI (`IDXGIAdapter3::QueryVideoMemoryInfo`) is possible over COM syscalls but needs
  hand-written vtable calls. It adds nothing over PDH plus the registry. Rejected.
- **NVIDIA extras (temperature, power):** use `nvidia-smi.exe` (in `C:\Windows\System32` with
  current drivers) only if it is present at probe time; see §1d.
- **Fallbacks (added 2026-10-02, t_f60987e4):** PDH failing used to mean "gpu: none
  detected" with the reason logged only at `Debug`. The probe now falls back to
  `nvidia-smi.exe` as a full backend (`windows-nvidia-smi`), then to adapter enumeration
  (`Win32_VideoController`, else the display-class registry key; name, vendor and VRAM only,
  `windows-adapters`). `nvidia-smi.exe` is resolved through `PATH`, then
  `%SystemRoot%\System32`, `%ProgramFiles%\NVIDIA Corporation\NVSMI`, and the DCH driver
  store (`System32\DriverStore\FileRepository\nv*`), because a service does not inherit the
  interactive user's `PATH`. Each failed step is listed in one startup `Warn` line.

### 1c. Linux (linux-box, linux-server, control plane, pi)

Enumerate `/sys/class/drm/card[0-9]+`, skipping connector entries like `card0-DP-1`.
Resolve `readlink device/driver` to learn the driver:

| Driver | Utilization | Memory | Temp / power | Unprivileged? |
|---|---|---|---|---|
| `amdgpu` | `device/gpu_busy_percent` | `device/mem_info_vram_used`, `mem_info_vram_total` (for an APU this is the carve-out; `mem_info_gtt_*` also exists) | `device/hwmon/hwmon*/temp1_input` (m°C), `power1_average` (µW; newer kernels `power1_input`) | yes, `0444` |
| `i915` | **no direct busy counter**. Derive from `gt/gt0/rc6_residency_ms` (older: `power/rc6_residency_ms`): `busy% ≈ 100·(1 − Δrc6_ms/Δwall_ms)`, clamped 0–100, and discard negative deltas (counter reset or wrap). | none (iGPU uses system RAM) | hwmon only on discrete Arc (DG2); freq from `gt/gt0/rps_act_freq_mhz` | yes (verified locally) |
| `xe` (newer Intel) | `device/tile0/gt0/gtidle/idle_residency_ms`, same delta formula | `device/tile0/vram0/…` on dGPU | hwmon on dGPU | yes |
| `nvidia` (proprietary) | no sysfs. `nvidia-smi --query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw --format=csv,noheader,nounits` | same | same | yes (needs `/dev/nvidia*` access, which is normally 0666) |
| `v3d`/`vc4` (Pi) | rpi kernels expose `gpu_stats` on the v3d platform device on recent kernels only; not portable | shared RAM | `/sys/class/thermal` is SoC, not GPU | — v1: report the GPU with name only, or skip it |

- The RC6-derived Intel value measures "GPU not in its idle power state." That is close
  to, but not the same as, `intel_gpu_top`'s per-engine busy. Accurate engine busy needs the
  i915/xe **PMU** through `perf_event_open`, which needs `CAP_PERFMON` or
  `perf_event_paranoid <= 0`. Debian ships `3`, which forbids unprivileged perf entirely.
  **`intel_gpu_top` therefore needs root or CAP_PERFMON** and is rejected. Granting
  `AmbientCapabilities=CAP_PERFMON` to the agent unit widens its privileges for a cosmetic
  gain. Rejected for v1.
- `/sys/class/powercap/intel-rapl*/energy_uj` is **root-only since 5.10** (CVE-2020-8694).
  No package or GPU power reading from RAPL.
- `amdgpu_top` and `radeontop` are not needed, because sysfs already gives what they give
  at this granularity.

### 1d. Power states: does reading wake a sleeping dGPU?

- **amdgpu:** sysfs and hwmon sensor reads on a **runtime-suspended** card return `EBUSY` and
  do **not** wake it. Each read on an **awake** card does restart its autosuspend timer
  (`power/autosuspend_delay_ms`, default ~5 s). Our 30 s tick is far longer than 5 s, so the
  card can still sleep between samples. Rule: read `device/power/runtime_status` first, and
  if it is `suspended`, emit the GPU with `util_percent = 0` and no other fields rather than
  touching sensors. Treat `EBUSY` as "suspended," not as an error.
- **nvidia-smi** *does* wake a runtime-D3 NVIDIA dGPU (Optimus/Turing+ laptops, and desktops
  with `NVreg_DynamicPowerManagement`). Same rule: on Linux, check the PCI device's
  `power/runtime_status` and skip the spawn when it is `suspended`. On Windows no equivalent
  cheap check exists. Desktop dGPUs there (windows-desktop) do not runtime-suspend, so accept it.
- **i915/xe (iGPU):** reading `rc6_residency_ms` or the freq files does not force the GT
  awake.
- **Apple / PDH:** passive reads of driver-published statistics have no wake effect.

### 1e. Spawn and CPU cost at 30 s

- Direct-read paths (Linux sysfs, darwin purego, Windows PDH): well under 1 ms per tick.
  Negligible.
- `nvidia-smi`: one process per tick, 20–100 ms with persistence mode on and up to ~1 s
  without it. At 30 s that is ≤ 3 % of one core in the worst case and typically ≪ 1 %.
  Acceptable, but it must run under the existing `cctx` timeout in `metricsLoop` with its
  own ~5 s cap, and it must never block the rest of the sample: collect GPU last, or in
  parallel with a deadline.

### 1f. No-GPU devices: zero-cost no-op

- **Probe once per process** (`sync.Once`) at the first GPU collect:
  - Linux: one `ReadDir("/sys/class/drm")`, plus `exec.LookPath("nvidia-smi")` only if an
    `nvidia` driver card exists.
  - darwin: `IOServiceGetMatchingServices(IOServiceMatching("IOAccelerator"))`, then check
    whether any entry has `PerformanceStatistics`.
  - Windows: open the PDH query and expand `\GPU Engine(*)\*`. Zero instances means no GPU.
- Probe result = a list of backends. **An empty list disables GPU collection for the life of
  the process.** There are no further syscalls or spawns and nothing is logged beyond one
  `Info` line ("gpu: none detected").
- A backend that fails **N=3 consecutive** times is disabled with one `Warn`, so a broken
  tool does not spam logs or burn a spawn every tick.
- Hot-plug (eGPU) needs an agent restart. That is acceptable for this fleet.

---

## 2. Wire protocol schema

### Shape: per-GPU array (recommended), not a single rollup

- Multi-GPU is real here: Linux boxes with iGPU+dGPU, Windows with iGPU+dGPU, and Windows
  also lists a Basic Render adapter, which we filter out. A rollup has to invent semantics
  (max util? summed VRAM across unrelated pools?) and loses the "which card is hot" signal.
- An array costs little: ≤ 4 entries × ~150 B on a ≤ 1 MiB envelope limit.
- The dashboard can still render a one-line rollup (the max `util_percent`) on `DeviceCard`.

```go
// proto/messages.go
const CapGPU = "gpu"

// GPU is one adapter's sample. Pointer fields are absent when the platform
// cannot report them (e.g. Apple unified memory has no VRAM total).
type GPU struct {
	Index         int      `json:"index"`
	Name          string   `json:"name"`
	Vendor        string   `json:"vendor"`                    // "apple","amd","intel","nvidia","unknown"
	UtilPercent   *float64 `json:"util_percent,omitempty"`
	MemUsedBytes  *uint64  `json:"mem_used_bytes,omitempty"`
	MemTotalBytes *uint64  `json:"mem_total_bytes,omitempty"`
	TempC         *float64 `json:"temp_c,omitempty"`
	PowerW        *float64 `json:"power_w,omitempty"`
	Suspended     bool     `json:"suspended,omitempty"`
}

type Metrics struct {
	// ...existing fields unchanged...
	GPUs []GPU `json:"gpus,omitempty"`
}
```

Why this departs from "plain values, zero on failure" *inside* `GPU`: the existing flat fields
exist on every OS, so zero there only means "failed." GPU fields are **structurally
unavailable** on some platforms: no VRAM total on Apple, no temperature on Intel iGPU, no
utilization on the first Windows tick. A `0 °C` or `0 B / 0 B` shown on the dashboard would be
wrong. Pointer plus `omitempty` is the Go idiom for "absent," and the repo already uses
pointers-for-absent (`registry.Device.Metrics *proto.Metrics`, `LastSeen *time.Time`) and
`omitempty` on the wire (`Envelope.Payload`). Field names follow the existing snake_case
unit-suffix convention (`_bytes`, `_percent`, `_s`), extended with `_c` and `_w`.

### Compatibility, both directions

| Pair | What happens | OK? |
|---|---|---|
| **new agent → old server** | The old hub's `json.Unmarshal` into the old `proto.Metrics` silently drops the unknown `gpus` key. No `DisallowUnknownFields` on the WSS path. | yes |
| **old agent → new server** | `gpus` is absent, so the slice is `nil` and re-marshals with `omitempty` as *absent* in SSE/API JSON. The TS type is `gpus?: GPU[]`. | yes |
| new agent without `gpu` cap / no GPU | `GPUs` stays nil and is omitted on the wire. Byte-identical to today's payload. | yes |
| new dashboard, old server | The API never sends `gpus`. The UI must treat `undefined` as "no GPU section." | yes |

### Version bump? **No.**

`Supported(v)` accepts `MinSupported <= v <= Version`, and it is evaluated only at handshake.
The change is purely additive and optional, and both directions degrade cleanly (table above),
so nothing needs negotiation. Bumping `Version` to 2 would actively hurt: every *old* server
(`Version = 1`) would reject new agents with `protocol_version_unsupported`, which forces a
server-first deploy order. It would also spend one slot of the N/N-1 window on a change that
does not need it. Keep `Version = 1` and `MinSupported = 1`. Add a `proto/messages_test.go`
round-trip test asserting that (a) a v1 payload without `gpus` decodes, and (b) a payload with
`gpus` decodes into a struct lacking the field (simulating the old server).

### Server-side sanitising (agent input is untrusted, per PLAN.md)

Add `func (m *Metrics) Sanitize()` in `proto`. The hub calls it on `TypeMetrics` before
`MetricsReceived`. It caps `len(GPUs)` at 8, truncates `Name`/`Vendor` to 64 bytes, clamps
`util_percent` to 0–100, and drops NaN/Inf floats. NaN would also make `json.Marshal` fail
when publishing SSE, so this is a correctness fix and not just hygiene.

---

## 3. Implementation touchpoints

**Go: proto**
- `proto/messages.go`: `CapGPU`, `GPU` struct, `Metrics.GPUs`, `Metrics.Sanitize()`.
- `proto/messages_test.go`: compat round-trips and sanitize tests.
- `proto/version.go`: **unchanged**.

**Go: agent**
- `agent/collectors/collectors.go`: make the collector stateful, because Windows keeps a PDH
  query open and Intel needs the previous RC6 sample for deltas. For example,
  `type Collector struct{ gpu gpuSource }`, `New(opts Options) *Collector`, and
  `(*Collector).Collect(ctx)`. GPU failure must **not** count toward `okCount`/`errNoCollectors`.
  GPU is best-effort and never fails the sample.
- New platform files. The existing `unix`/`windows` split is too coarse, because Linux and
  darwin differ:
  - `agent/collectors/gpu.go`: shared types, probe-once, the disable-after-N-failures wrapper,
    and `runtime_status` helpers.
  - `gpu_linux.go`: drm/sysfs backends (amdgpu, i915, xe) and the nvidia-smi backend.
  - `gpu_darwin.go`: purego IOKit `IOAccelerator` → `PerformanceStatistics`.
  - `gpu_windows.go`: PDH plus registry.
  - `gpu_other.go` (`//go:build !linux && !darwin && !windows`): no-op, so `go vet` and any
    future GOOS still compile.
  - Tests: `gpu_linux_test.go` with a fake sysfs root in `t.TempDir()`, so the sysfs root path
    must be injectable. Add a PDH instance-name parser test and an aggregation test that are
    OS-independent (pure functions in `gpu.go`).
- `agent/app/app.go`: build the `Collector` once per `App` (not per connection), pass
  `GPU: a.has(proto.CapGPU)`, and use it in `metricsLoop`.
- `cmd/kuma-agent/main.go` (selftest): switch to `collectors.New(...)`. The GPU probe must not
  fail the selftest, or else a flaky GPU would block upgrades and trigger rollback.
- `go.mod`: promote `github.com/ebitengine/purego` from indirect to direct.

**Go: server**
- `server/hub/hub.go`: call `m.Sanitize()` in the `TypeMetrics` case.
- `server/registry/ring.go` and `registry.go`: **no code change**. `Ring` stores `proto.Metrics`
  by value, and the `GPUs` slice rides along (freshly decoded per message and never mutated, so
  aliasing is safe). Memory: 7 devices × 60 samples × ≤ 4 GPUs is negligible.
- **`server/store/`: no migration.** Metrics are not persisted. `capabilities_json` already stores
  arbitrary capability strings.
- `internal/integration/metrics_test.go`: end-to-end test that a `gpus` array from an agent
  reaches `GET /api/devices/{name}/metrics` and the SSE `metrics` event. The GPU source must be
  injectable in the agent `Collector`, because CI hosts may have no GPU.

**Web**
- `web/src/api.ts`: `interface GPU { index: number; name: string; vendor: string; util_percent?: number; mem_used_bytes?: number; mem_total_bytes?: number; temp_c?: number; power_w?: number; suspended?: boolean }`
  and `gpus?: GPU[]` on `Metrics`.
- `web/src/components/DeviceCard.tsx`: one grid cell per GPU (or the max-util rollup when there
  are more than 2), for example `GPU: 37% · 2.1 GB / 8.0 GB · 61°C`. Omit absent parts, and show
  "GPU: asleep" when `suspended`.
- `web/src/views/DeviceDetail.tsx`: a `Sparkline` per GPU index,
  `history.map(m => m.gpus?.find(g => g.index === i)?.util_percent ?? 0)`, rendered only if the
  latest sample has that GPU. `Sparkline.tsx` needs no change. Maybe widen its `w-14` label.
- `web/src/state.ts`: no change (it passes `metrics` through). Add a `state.test.ts` case for a
  metrics event carrying `gpus`.

**Config and docs**
- `configs/kuma-agent.*.example.yaml`: add `gpu` to the capabilities of GPU devices (macos-workstation,
  macos-desktop, windows-desktop, linux-server/linux-box if they have a GPU).
- `docs/DEVICE-SETUP.md`: document the `gpu` capability and the per-OS data sources and limits.
- `docs/PLAN.md`: note the `gpus` field in the metrics payload and the protocol table.

---

## 4. Cost and risk summary

| Risk | Mitigation |
|---|---|
| Spawn cost | Only NVIDIA uses a spawn (`nvidia-smi`). It is absolute-path, resolved once, under a 5 s timeout, and skipped when the device is runtime-suspended. Every other path is direct reads. |
| Waking dGPUs | Check `power/runtime_status` before any Linux sensor read or `nvidia-smi` call. The 30 s tick exceeds the amdgpu autosuspend delay (~5 s). |
| GPU-less devices | Probe once, then a nil backend list means zero work. The `gpu` capability also gates it. |
| Upgrade safety | A GPU error never fails `Collect` or the selftest, so no rollback is ever triggered by GPU code. |
| purego / unsafe code on darwin | Mirror gopsutil's already-shipping pattern. `CFRelease` every created object. Fuzz nothing. Verify on both Macs. Keep `ioreg -r -d 1 -c IOAccelerator` as the cross-check. |
| Windows PDH under a non-admin service in session 0 | Verify on windows-desktop, and compare against Task Manager under load. Fallback if it reads low: document that the service account may need "Performance Log Users" (still not admin). |
| Intel util is approximate (RC6) | Document the approximation in DEVICE-SETUP. The accurate path (CAP_PERFMON) is deliberately not taken. |
| Untrusted agent payload | `Metrics.Sanitize()` on the server: caps, clamps, NaN/Inf removal. |
| Protocol compat | Additive `omitempty` field and no version bump. Covered by proto round-trip tests. |

---

## Sources

- IORegistry `PerformanceStatistics` / `Device Utilization %`, unprivileged:
  [simonw/gpuer](https://github.com/simonw/gpuer),
  [fak #11319](https://github.com/anthony-chaudhary/fak/issues/11319),
  [toptop #4](https://github.com/ur-grue/toptop/issues/4),
  [MacRumors ioreg IOAccelerator thread](https://forums.macrumors.com/threads/request-share-your-ioreg-ioaccelerator-results-please.2293664/)
- purego IOKit without cgo: gopsutil v4.26.8 `internal/common/common_darwin.go` (local module cache).
- PDH GPU Engine counters: [oshi PR #2114](https://github.com/oshi/oshi/pull/2114),
  [fecf gist: get GPU utilization](https://gist.github.com/fecf/2103a82afc76b5c88829c4383944a5aa),
  [MS Q&A: GPU usage with PDH](https://learn.microsoft.com/en-us/answers/questions/5641645/how-to-get-the-special-process-gpu-usage-with-the);
  pdh.dll via x/sys LazyDLL: gopsutil `internal/common/common_windows.go`.
- amdgpu sysfs and runtime PM: [kernel amdgpu thermal/PM docs](https://docs.kernel.org/gpu/amdgpu/thermal.html),
  [omastats #9](https://github.com/crmne/omastats/issues/9) / [#10](https://github.com/crmne/omastats/pull/10),
  [glances #3590 (EBUSY when suspended)](https://github.com/nicolargo/glances/issues/3590),
  [lunnova: GPU runpm spurious resumes](https://lunnova.dev/articles/linux-gpu-runpm-spurious-resumes/)
- i915 RC6, RAPL, and perf_event_paranoid: verified directly on the control-plane host (see §0).

---

## Decisions

1. **Collection method per OS:** direct reads, no cgo, and spawns only where unavoidable.
   - **macOS:** IOKit `IOAccelerator` → `PerformanceStatistics` (`Device Utilization %`,
     `In use system memory`, `model`) via **purego** (already in go.sum through gopsutil).
     No `powermetrics` (root), no `ioreg` spawn, and no temperature/power in v1.
   - **Windows:** PDH `GPU Engine` (Task-Manager aggregation: per LUID, sum per engtype, max
     over engtypes) and `GPU Adapter Memory` via `pdh.dll` syscalls, with a persistent query.
     Name and VRAM total come from the display-class registry key. `nvidia-smi.exe` is used
     for temperature/power only if present.
   - **Linux:** `/sys/class/drm` per driver. amdgpu uses `gpu_busy_percent`, `mem_info_vram_*`,
     and hwmon. i915/xe busy is derived from RC6/gtidle residency deltas. NVIDIA uses
     `nvidia-smi` CSV if present. Every read is guarded by `power/runtime_status`. No
     `intel_gpu_top`, no CAP_PERFMON, no RAPL.
2. **Schema:** a per-GPU array, `Metrics.GPUs []GPU json:"gpus,omitempty"`. The `GPU` struct
   uses pointer + `omitempty` for platform-optional numerics (`util_percent`, `mem_used_bytes`,
   `mem_total_bytes`, `temp_c`, `power_w`), plus `index`, `name`, `vendor`, `suspended`. The
   server sanitizes the payload through `Metrics.Sanitize()`.
3. **Version bump:** **no.** `Version` and `MinSupported` stay at 1. The change is additive,
   and unknown fields are ignored on the WSS path. Bumping would lock out new agents on old
   servers.
4. **Config gating:** yes. Add a new `gpu` capability (`proto.CapGPU`). It takes effect only
   together with `metrics`, because it rides in the metrics message. When the capability is
   present but no GPU is found, the agent probes once, caches "none," and does nothing
   afterwards.
5. **Storage:** no SQLite migration. GPU data lives in the existing in-memory 60-sample
   registry ring with no ring code changes.
6. **Failure isolation:** GPU collection never fails `Collect` or the pre-upgrade selftest.
   A backend is disabled after 3 consecutive failures.

## Open questions for operator

None.
