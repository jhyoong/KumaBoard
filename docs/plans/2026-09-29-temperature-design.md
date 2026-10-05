# Device temperature: design

- Date: 2026-09-29
- Status: proposed
- Scope: agent collector → `proto.Metrics` → SQLite history → dashboard card
  and device detail page. GPU temperature (`proto.GPU.TempC`) already exists and
  is out of scope. This design only adds a matching host-level field.

## Problem statement

Dashboard metrics cover CPU %, memory, disk, uptime, load and GPUs. Of these,
only GPUs report a temperature (`gpu.go` via nvidia-smi `temperature.gpu`,
`gpu_linux.go:readHwmon` via the card's `hwmon*/temp1_input`, shown by
`web/src/gpu.ts`). There is no CPU/SoC/board temperature, so we can't see a
fanless control plane host or a Pi that is throttling. The goal is one host temperature per
device. It is sampled on the existing metrics tick, stored alongside the other
history columns, and shown next to CPU/RAM with warn/crit colouring and a 1h
sparkline.

Constraints from the codebase:

- `proto` must not import `server/` or `agent/`. Versioning uses the N/N-1
  window (`proto/version.go`: `Version = 1`, `MinSupported = 1`,
  `Supported(v) = MinSupported <= v <= Version`).
- `CGO_ENABLED=0`. Platform code is split into `_linux`/`_darwin`/`_windows`/
  `_other` files (see `gpu_*.go`, `root_unix.go`/`root_windows.go`). Both
  `GOOS=windows` and `GOOS=darwin` builds must keep compiling.
- The "absent is omitted, never a fake zero" convention already covers GPU
  fields (pointer + `omitempty` in `proto.GPU`, nullable SQLite columns,
  `?? null` gaps in `history.ts`). Temperature follows it.

## Decisions

### D1. Semantics: one scalar + sensor label

`temp_c` is a single host temperature in °C, taken from the best CPU/SoC
sensor (priority rules in D2). `temp_sensor` is a short free-text label naming
where the reading came from, e.g. `coretemp/Package id 0`, `k10temp/Tctl`,
`cpu_thermal/temp1`, `PMU tdie3`. Both are optional.

"Best CPU sensor" means *chip priority first, then the package label or the
maximum within that chip*. It is not "global max over every sensor", which
would pick up NVMe, Wi-Fi or GPU chips that are already covered by the GPU path.

- Rejected: a per-sensor list (`[]TempSensor`). It gives the card nothing
  simple to show, needs a list cap and sanitising, and history can't store it
  as a column.
- Rejected: a global max over all sensors. NVMe and Wi-Fi sensors run hot and
  would dominate the reading; GPU sensors would be double-counted.

### D2. Agent collection per OS

The new files follow the GPU layout. `temp.go` holds pure selection logic
(testable on every OS), and each platform file exports one
`probeTemp(opts Options) tempSource` (nil = unsupported):

| File | Build tag | Contents |
|---|---|---|
| `agent/collectors/temp.go` | none | `tempSource` interface, `tempCollector` (probe once, `failGate` after `gpuMaxFailures`), `pickHwmon(chips []hwmonChip) (float64, string, bool)`, `pickDarwin([]sensors.TemperatureStat)`, and plausibility filter `plausibleTemp(v float64) bool` (5 < v < 150) |
| `temp_linux.go` | (filename) | sysfs walker over `Options.HwmonRoot` (default `/sys/class/hwmon`), plus a thermal_zone fallback over `Options.ThermalRoot` (default `/sys/class/thermal`) |
| `temp_darwin.go` | (filename) | wraps `github.com/shirou/gopsutil/v4/sensors.TemperaturesWithContext` |
| `temp_windows.go` | (filename) | `func probeTemp(Options) tempSource { return nil }` |
| `temp_other.go` | `!linux && !darwin && !windows` | same nil stub, like `gpu_other.go` |

`Collector.Collect` samples temperature after the host collectors and before
GPUs. A temperature failure never counts toward `okCount` and never produces
an error, which matches the GPU rule. No new capability is added (see D2d).

#### D2a. Linux (x86 and Pi)

Write a small in-house walker rather than calling `gopsutil/sensors`. gopsutil
flattens results to `chip_label` keys and drops the device path, which makes
GPU/NVMe filtering fragile. It is also hard to point at a fixture tree.
`gpu_linux.go` already has `readUint`/`readString`, and `DRMRoot` sets the
precedent for fixture roots.

Algorithm (runs every tick; it is a handful of small sysfs reads):

1. For each `<HwmonRoot>/hwmon*`, read `name`. Skip the chip if the name is in
   the exclusion set: `amdgpu nouveau nvidia radeon i915 xe` (GPU path), `nvme
   drivetemp` (disks), `iwlwifi* mt7921* ath* brcmfmac*` (radios), `spd5118
   jc42` (DIMMs), `BAT*` `ucsi*` (power). Also skip the chip if its `device`
   symlink resolves under a `/drm/` card that the GPU collector owns. That is
   belt-and-braces for oddly named GPU hwmons.
2. Rank the remaining chips by name:
   `coretemp` (Intel) = `k10temp` = `zenpower` (AMD) > `cpu_thermal`
   `cpu-thermal` `soc_thermal` (Pi and other ARM SoCs) > `acpitz` > anything
   else. If nothing ranks, the result is absent.
3. Within the top chip, read every `temp*_input` (m°C → °C) and its
   `temp*_label`:
   - `coretemp`: use `Package id 0` if present, else the max of `Core *`.
   - `k10temp`: use `Tdie` if present (older Ryzen/TR, where `Tctl` carries an
     offset), else `Tctl`. Ignore `Tccd*` for the scalar.
   - otherwise: use the max of the chip's inputs.
   - If there are several same-rank chips (dual-socket `coretemp`), take the max.
4. Drop any value that fails `plausibleTemp`. A raw `0`, negative values and
   `-EIO`/`ENODATA` read errors all mean "no reading".
5. If no hwmon chip qualifies, fall back to `<ThermalRoot>/thermal_zone*/`
   where `type` is `x86_pkg_temp`, `cpu-thermal`, `cpu_thermal` or `soc_thermal`
   (in that order), reading `temp` (m°C). This covers kernels where the thermal
   zone has no hwmon (gopsutil hits the same Raspbian quirk, issue #391).
6. Nothing found at the first probe ⇒ log `temp: no sensor detected` once and
   disable. Values that fail on 3 consecutive ticks ⇒ disable via `failGate`,
   matching GPUs.

Pi: a stock Raspberry Pi OS kernel normally exposes `cpu_thermal` (bcm2835/
bcm2711/bcm2712 thermal) as both hwmon and thermal_zone0, so the Pi is expected
to be *supported*. The task brief says our Pi has no CPU temperature sensor.
If that holds for our box (container, custom kernel, missing module), step 6
applies and the field is omitted. It must never be sent as `0`. See open
question Q1.

#### D2b. macOS

This is supported (best-effort) without root. `powermetrics` needs root, and
the agent's unix side has no root convention to lean on. `root_unix.go` only
defines the disk root path `/` and `errNoCollectors`; there is no privilege
helper. So `powermetrics` is rejected.

Instead, use `gopsutil/v4/sensors`, which is already in the module graph
(v4.26.8):

- `darwin && arm64` (`sensors_darwin_arm64.go`) reads
  `IOHIDEventSystemClient` through purego. It is unprivileged and has no cgo,
  which is the same approach as our `gpu_darwin.go` IOKit reader.
- `darwin && !arm64` (`sensors_darwin.go`) reads SMC keys through IOKit,
  also unprivileged.

Selection (`pickDarwin`): Apple Silicon uses the max over sensors whose key
starts with `PMU tdie` (CPU die sensors), else `PMU TP*`/`pACC`/`eACC` MTR
sensors, else absent. Intel uses the SMC CPU proximity/die key (`TC0P`/`TC0D`/
`TC0E`/`TC0F`, gopsutil key names), else absent. Then apply `plausibleTemp`.

Because the IOHID route is a private API, wrap the call like `safeSample`,
with panic recovery, a 2s context timeout and the `failGate`. If the sensor
names change in a future macOS release, the field silently disappears; it
never shows a wrong zero.

- Rejected: `powermetrics` via sudo/setuid. It needs a root policy the agent
  doesn't have and spawns a heavy process on every tick.
- Rejected: declaring macOS unsupported. An unprivileged, cgo-free path
  already exists in a dependency.

#### D2c. Windows

Unsupported in v1. `temp_windows.go` returns a nil source, so the field is
always omitted.

> **Superseded 2026-10-02 (t_f60987e4, operator request).** `temp_windows.go`
> now probes three driver-free WMI sources in order and keeps the first with a
> plausible reading: `MSAcpi_ThermalZoneTemperature` (root\WMI, tenths of K,
> usually admin-only), `Win32_TemperatureProbe` (root\cimv2, rarely
> populated), then the "Thermal Zone Information" performance counters
> (`Win32_PerfFormattedData_Counters_ThermalZoneInformation`, readable without
> admin). These are still ACPI zones, not the CPU package, so the sensor label
> names the source and zone. When all three fail, one startup `Warn` line lists
> each probe and why it failed. The ring-0 driver rejection below stands.

`MSAcpi_ThermalZoneTemperature` (the only driver-free source, and what gopsutil
uses) reports an ACPI thermal zone, not the CPU. On most desktop boards it is
missing or returns a constant (often 27.8 °C, the firmware default). It also
needs a per-tick WMI/COM round-trip. Real package temperatures on Windows need
a ring-0 driver (WinRing0/LibreHardwareMonitor), which is a known-vulnerable
driver class that we will not ship.

- Rejected: WMI ACPI zone. Its readings are unreliable, and a plausible but
  wrong number is worse than none.
- Rejected: bundling LibreHardwareMonitor/WinRing0. It brings a kernel driver
  and its CVE history.

#### D2d. Capability gating

Temperature is collected whenever `CapMetrics` is on. No `CapTemp` is added.

- Rejected: a new capability like `CapGPU`. `CapGPU` exists because
  nvidia-smi spawns are costly and can wake suspended cards. A few sysfs/IOHID
  reads carry neither risk, and a new capability would mean editing seven
  device configs.

### D3. Wire format

Add two optional fields to the end of `proto.Metrics`. Nothing is renamed, and
`Version`/`MinSupported` stay at 1.

Compatibility check against `Supported()` and the decoders:

- An old agent on a new server sends no `temp_c`. The pointer stays nil, and
  the fields are omitted when re-encoded to SSE/API (same as
  `TestMetricsWithoutGPUsDecodes`).
- A new agent on an old server: `Envelope.Unmarshal` is a plain
  `json.Unmarshal` without `DisallowUnknownFields`, so the unknown keys are
  ignored (same as `TestMetricsWithGPUsDecodesIntoOldStruct`).
- The handshake never looks at metrics fields. `Supported()` only compares
  `hello.protocol_version`, so no bump is needed. A bump would be the wrong
  tool here: it would reject nothing and would burn the N-1 slot.

```go
// proto/messages.go
type Metrics struct {
	// ... existing fields unchanged ...
	GPUs []GPU `json:"gpus,omitempty"`
	// TempC is the host CPU/SoC temperature in °C; absent when the platform
	// has no usable sensor (never sent as 0).
	TempC *float64 `json:"temp_c,omitempty"`
	// TempSensor names the sensor TempC came from, e.g. "k10temp/Tctl".
	TempSensor string `json:"temp_sensor,omitempty"`
}

// Sanitize additions (server side, untrusted input):
const MaxTempSensorLen = 64 // same bound as MaxGPUTextLen

m.TempC = finitePtr(m.TempC)
if m.TempC != nil && (*m.TempC < -50 || *m.TempC > 200) { m.TempC = nil }
if m.TempC == nil { m.TempSensor = "" }
m.TempSensor = truncateText(m.TempSensor, MaxTempSensorLen)
```

The server-side sanity window (−50…200) is deliberately wider than the agent's
`plausibleTemp`. Its job is to keep junk out of the database, not to second-guess
sensors.

Example payloads:

```json
{"cpu_percent":12.5,"mem_total_bytes":16777216000,"mem_used_bytes":6291456000,
 "mem_used_percent":37.5,"disk_total_bytes":512110190592,"disk_used_bytes":98784247808,
 "disk_used_percent":19.3,"uptime_s":86400,"load1":0.4,"load5":0.3,"load15":0.2,
 "temp_c":54,"temp_sensor":"k10temp/Tctl"}
```

```json
{"cpu_percent":3.1, "...": "...", "load15":0.1}
```

The second payload is a Windows or sensorless host: no `temp_c` and no
`temp_sensor` keys, never `"temp_c":0`.

`web/src/api.ts` `Metrics` gains `temp_c?: number; temp_sensor?: string;`.

### D4. Storage

Migration `server/store/migrations/0005_metrics_temp.sql`. The same pattern as
0004 applies: nullable, no default, so existing rows read as NULL ("unknown"),
not 0.

```sql
-- Host CPU/SoC temperature in °C. NULL when the device reported none
-- (Windows, sensorless hosts, pre-temperature agents). The sensor label is
-- live-only and not stored.
ALTER TABLE metrics_samples ADD COLUMN temp_c REAL;
ALTER TABLE metrics_rollup ADD COLUMN temp_c REAL;
```

The existing `migrate()` loop in `store.go` applies it to existing databases
on the next server start, inside its own transaction. `ALTER TABLE ADD COLUMN`
with a nullable column is O(1) in SQLite and works on `WITHOUT ROWID` tables.

Code changes in `server/store/metrics.go`:

- `MetricsSample.TempC *float64`.
- `RecordMetrics`: add `temp_c` to the INSERT column list, VALUES and
  `ON CONFLICT ... SET temp_c = excluded.temp_c`, bound to `m.TempC` (a nil
  pointer binds NULL).
- `rollupSelect`: `AVG(s.temp_c) AS temp`. `AVG` skips NULLs, so a rollup
  bucket is NULL only when no sample in it had a temperature, the same
  semantics as `gpu`. Add `temp_c` to the `INSERT OR REPLACE INTO
  metrics_rollup (...)` column list in `RollupMetrics`, and to both SELECT
  arms of `MetricsRollupHistory` and `MetricsHistory`.
- `queryHistory`: scan into `sql.NullFloat64`.
- Retention: unchanged and automatic. Samples are kept for 2h at 30s
  (`MetricsHotRetention`) and folded into 15-minute rollups kept for 30 days
  (`MetricsRetention`), via the same `RollupMetrics`/`DeleteMetricsBefore`
  calls in `cmd/kumaboard/serve.go`.

API (`server/api/devices.go` `historySample`): add
`TempC *float64 \`json:"temp_c,omitempty"\``; `web/src/api.ts`
`HistorySample` gains `temp_c?: number`.

- Rejected: storing `temp_sensor` per row. It is effectively constant per
  device, adds bytes to every row and doesn't aggregate.
- Rejected: `MAX` instead of `AVG` for rollups. It is inconsistent with every
  other rolled-up column. See Q3.

### D5. UI

Thresholds live in a new `web/src/temp.ts` (pure, vitest-covered, like
`gpu.ts`):

```ts
export const TEMP_WARN_C = 80;
export const TEMP_CRIT_C = 95;
export type TempLevel = 'ok' | 'warn' | 'crit';
export function tempLevel(c: number): TempLevel;            // >=95 crit, >=80 warn
export function tempText(c: number): string;                // "54°C"
export function tempClass(c: number): string;                // '', 'text-warning', 'text-danger font-medium'
```

Why 80/95: Raspberry Pi firmware soft-throttles at 80 °C (Pi 4/5), so
reaching warn there means the Pi is actively throttling. Intel TjMax is
typically 100 °C and AMD's Tctl limit is 95 °C, so 95 is where x86 parts hit
their own throttle point. Sustained load below 80 is normal for every device
in the fleet. Known false positive: Ryzen 7000 desktop parts boost to 95 °C by
design under all-core load, so they will flash crit during heavy work. That is
accepted for v1 (see Q2).

The colours use the existing `--color-warning` / `--color-danger` tokens
(`index.css`), so light/dark theming is inherited.

**Main dashboard card** (`components/DeviceCard.tsx`, the 2-column metrics
grid). Insert a `Temp` cell right after `Mem`. It is rendered only when
`m.temp_c !== undefined`, so Windows and sensorless cards keep today's layout.
The label rides in the cell's `title` tooltip.

```
┌ linux-server ─────────────────────────── [online] ┐
│ linux/amd64 · v0.4.0                        │
│ Last seen: 12s ago                          │
│                                             │
│ CPU: 12.5%            Mem: 5.9 GB / 15.6 GB │
│ Temp: 54°C            Disk: 92 GB / 477 GB  │   ← Temp coloured by tempClass
│ Up: 1d 0h             Load: 0.40/0.30/0.20  │
│ GPU: 37% · 2.1 GB / 8.0 GB · 61°C · 118 W   │
│                                             │
│ CPU 1h  ╱╲__╱‾‾╲_            12%            │
│ RAM 1h  ‾‾‾‾‾‾‾‾‾            38%            │
│ Temp 1h __╱‾‾╲___            54°C           │   ← new, only if any temp in window
│ GPU 1h  _╱╲_____             37%            │
└─────────────────────────────────────────────┘
```

**1h sparkline**: reuse `components/Sparkline.tsx` inside `HistorySparklines`.
Two small, backwards-compatible changes to `Sparkline` are needed:

- An optional `unit` prop (default `'%'`). Today the `%` in the readout is
  hard-coded.
- An optional `color` for the readout (to pass `tempClass`).

Use `max={110}` so a 95 °C crit reading isn't pinned to the top edge. In
`history.ts`:

- `mergeLive` copies `m.temp_c` into `live.temp_c` when defined.
- `historySeries` gains `temp: (number | null)[] | null`: null when no sample
  in the window has a temperature, and null per point for gaps (never 0,
  following `gpuMem`).

`Sparkline` takes `number[]`, so `HistorySparklines` passes the non-null
points only (gaps collapse). On a 1h card view that is acceptable. The detail
chart shows true gaps.

**Device detail page** (`views/DeviceDetail.tsx`):

- `CurrentMetrics`: add a row inside the existing **CPU** `StatCard`, under
  `Load`: `Temp 54°C · k10temp/Tctl`, coloured by `tempClass`. It is omitted
  when absent. This keeps the `lg:grid-cols-4` stat grid intact (a fifth card
  would wrap awkwardly) and places temperature next to CPU.

  ```
  ┌ CPU ────────────────────────┐
  │ 12.5%                       │
  │ Load 0.40 / 0.30 / 0.20     │
  │ Temp 54°C · k10temp/Tctl    │
  └─────────────────────────────┘
  ```
- `MetricsCharts`: add a `TimeSeriesChart` with `label="Temperature °C"`,
  `values={s.temp ?? []}`, `max={110}`, `format={(v) => \`${v.toFixed(0)}°C\`}`,
  `color="var(--color-chart-3)"`, and empty text `'No temperature reported in
  this range'`. `TimeSeriesChart` already accepts `(number | null)[]` (GPU
  memory uses it) and draws the gaps.
- GPU cards are unchanged.

### D6. Threshold configurability

v1 uses server-agnostic constants in `web/src/temp.ts`, the same for every
device. No config, no API and no wire change.

- Rejected: per-agent thresholds in the agent config. They would add wire
  fields, the value belongs to the viewer rather than the sensor, and they
  would spread one policy across seven configs.
- Rejected: server `config.yaml` thresholds. They would need plumbing through
  an API to the SPA for a value nobody has asked to change yet. Revisit as a
  per-device override if Q2 bites (see Q2).

## Per-OS support

| Target | Supported (v1) | Source | Notes |
|---|---|---|---|
| Linux x86 (linux-box, linux-server) | Yes | `/sys/class/hwmon/*` chips `coretemp` (Package id 0) or `k10temp`/`zenpower` (Tdie→Tctl); thermal_zone `x86_pkg_temp` fallback | GPU/NVMe/radio/DIMM chips excluded; runs unprivileged |
| Linux Pi (pi) | Yes if the kernel exposes it; else omitted | hwmon `cpu_thermal`, fallback `thermal_zone*/type=cpu-thermal` | Brief says our Pi lacks the sensor: then `temp_c` is omitted and never 0 (Q1) |
| macOS (macos-workstation, macos-desktop) | Yes, best-effort | `gopsutil/v4/sensors`: IOHID `PMU tdie*` (arm64) / SMC `TC0*` (Intel), purego, no root | `powermetrics` rejected (root). Private API, so failGate + panic guard; disappears rather than lying |
| Windows (windows-desktop, windows-headless) | No | none (`temp_windows.go` stub) | ACPI WMI zone unreliable or constant; ring-0 drivers rejected |

## Test plan

Agent unit tests (`agent/collectors`, OS-independent where possible):

- `temp_test.go` (no build tag): tests `pickHwmon` against table inputs:
  - coretemp package vs cores
  - k10temp with and without Tdie
  - `cpu_thermal` only
  - `amdgpu` + `nvme` only ⇒ absent
  - dual coretemp ⇒ max
  - `0`/negative/`200000` m°C ⇒ dropped
  - no chips ⇒ absent

  Also tests `pickDarwin` against a slice of `sensors.TemperatureStat`, and
  `tempCollector` probe-once and `failGate` disable, mirroring
  `TestGPUProbeOnceNone` / `TestGPUBackendDisabledAfterFailures`.
- `temp_linux_test.go`: fixture trees under `t.TempDir()` in the style of
  `fakeDRMCard`/`writeSysfs`. The fixtures cover hwmon with a `name` + `temp*_input`
  + `temp*_label`, and a thermal_zone-only tree (the Pi fallback). Collect
  through `Options{HwmonRoot, ThermalRoot}`.
- `TestCollectNeverFailsOnTemp`: a broken temperature source doesn't turn
  `Collect` into an error.

Proto (`proto/messages_test.go`):

- A pre-temp payload decodes with `TempC == nil` and re-encodes without
  `temp_c`/`temp_sensor`.
- A payload with a temperature decodes into the old struct shape.
- `Sanitize` drops NaN/Inf/out-of-window values, clears `TempSensor` when
  `TempC` is nil, and truncates the label.

Store (`server/store/metrics_test.go`):

- `RecordMetrics` with and without temp ⇒ `MetricsHistory` returns
  pointer/nil.
- A rollup bucket mixing nil and 50/60 ⇒ 55. An all-nil bucket ⇒ NULL.
- Migration test: open a DB at schema 0004, insert rows, reopen ⇒ 0005
  applied and old rows read `TempC == nil`.

Integration (`internal/integration/`, real server + agent over TLS, temp
SQLite, `harness_test.go`):

- New `temp_linux_test.go`, modelled on `TestGPUMetricsEndToEnd`: a
  `fakeHwmon(t)` fixture (k10temp `Tctl=54000`, plus an `nvme` chip at 70000
  that must be ignored) goes to
  `a.SetCollector(collectors.New(collectors.Options{HwmonRoot: ..., Log: h.log}))`.
  The test asserts `temp_c:54`/`temp_sensor:"k10temp/Tctl"` on SSE
  (`sseMetrics`/`nextSSEMetrics`), on `apiMetrics`, and in `metricsHistory`
  for `1h` and a rolled-up window (as in `TestGPUMemoryHistoryEndToEnd`).
- An old agent on a new server: extend `TestOldAgentMetricsOnNewServer` to
  assert that no `temp_c` key appears in SSE or history.
- A sensorless agent (empty `HwmonRoot`): metrics flow and `temp_c` is absent
  end-to-end.

Web (vitest):

- `temp.test.ts`: boundaries 79.9/80/94.9/95.
- `history.test.ts`: `mergeLive` carries `temp_c`; `historySeries.temp` is
  null when absent everywhere and null-gapped when absent in some samples.
- A `Sparkline` unit prop test if the component gains tests.

Cross-compile gate: `go vet ./...`, `GOOS=windows GOARCH=amd64 go build ./...`,
`GOOS=darwin GOARCH=arm64 go build ./...`, `GOOS=darwin GOARCH=amd64 go build
./...`, `GOOS=linux GOARCH=arm64 go build ./...`. Then run `make all`.

Manual: `make deploy-<device>` to one device per row of the support table.
Compare against `sensors` (Linux), `sudo powermetrics --samplers smc` (macOS,
check only) and confirm Windows cards show no Temp cell.

## Open questions

- **Q1 (Pi):** does our Pi actually lack `/sys/class/hwmon/*/name ==
  cpu_thermal` and `/sys/class/thermal/thermal_zone0`? Stock Pi OS has both.
  If they are missing, find out why (container? kernel?) before calling it
  unsupported. The design handles both outcomes.
- **Q2 (thresholds):** Ryzen 7000 and Apple Silicon under load routinely sit
  at 90–100 °C. If crit flapping is noisy, add per-device overrides (server
  side, stored on `devices`) in a follow-up rather than changing the global
  defaults.
- **Q3 (rollup):** should the 24h/7d/30d chart show a peak (`MAX`) alongside
  the average, so short thermal spikes are visible? v1 uses `AVG` only.
- **Q4 (macOS sensor names):** confirm the `PMU tdie*` keys on the actual
  M-series Macs (and whether the macOS desktop is Intel). If gopsutil returns
  nothing usable, macOS falls back to "omitted" automatically.
- **Q5:** should the dashboard show a small "no sensor" hint on Windows cards,
  or stay silent (current proposal: silent)?

## Implementation-card handoff checklist

Order the cards so that each one leaves the tree green. Each card: `gofmt -w`,
`go vet ./...`, `make test`, `GOOS=windows GOARCH=amd64 go build ./...`.

1. **proto**:
   - add `TempC`, `TempSensor` and `MaxTempSensorLen` to `Metrics`, plus the
     `Sanitize` rules
   - tests in `messages_test.go`
   - no `Version` change
2. **store**:
   - migration `0005_metrics_temp.sql`
   - `MetricsSample.TempC`
   - update `RecordMetrics`, `rollupSelect`, `RollupMetrics`,
     `MetricsHistory`, `MetricsRollupHistory` and `queryHistory`
   - store tests, including the migration from 0004
3. **api**:
   - `historySample.TempC`
   - extend `metrics_history_test.go`
4. **agent collectors**:
   - `temp.go`, `temp_linux.go`, `temp_darwin.go`, `temp_windows.go`,
     `temp_other.go`
   - `Options.HwmonRoot`/`ThermalRoot`
   - wire into `Collect` (not counted in `okCount`)
   - unit and fixture tests
   - darwin arm64/amd64 cross-builds
5. **integration**:
   - `internal/integration/temp_linux_test.go`
   - extend `TestOldAgentMetricsOnNewServer`
6. **web data**:
   - `api.ts` types
   - `temp.ts` + tests
   - `history.ts` (`mergeLive`, `historySeries.temp`) + tests
7. **web UI**:
   - `Sparkline` `unit`/readout colour
   - `HistorySparklines` Temp 1h
   - `DeviceCard` Temp cell
   - `DeviceDetail` CPU card row + Temperature chart
   - `npm run lint`, `npm test`, `npm run build`
8. **Rollout**:
   - `make all`
   - deploy the server first (the new server accepts old agents), then the
     agents
   - verify one device per support-table row
   - resolve Q1/Q4 with real readings
