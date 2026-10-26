# Ambient-aware Fan Control Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking. The user requested execution in this thread without other agents or threads.

**Goal:** Add disk-led automatic cooling with cupboard sensor input and a ten-minute minimum between ordinary speed changes.

## Implementation status, 2026-10-26

Policy, readers, SMART helper, controller, HTTP, UI, documentation and local deployment preparation are implemented. Go race tests, vet, Python fixture tests, Linux/amd64 cross-build, both affected kustomizations and browser interactions pass. Manual, automatic, fallback, dry-run, write-error and narrow-screen captures were inspected. Following user approval, the restricted helper is installed and image `ambient-20261026-4cfd483d0d82` is published at digest `sha256:4bdec6e4f13447eed69181982391bcff7ef8547a6d088afecb060c27e1724fac`.

Docker Desktop's proxy DNS prevented the normal Dockerfile build. Host-side crane appended the cross-compiled binary to the existing immutable amd64 runtime image, retaining its certificates, timezone data and entrypoint. The running binary hash matches the local build. A separate `unas-fan-controller-dryrun` deployment uses auto mode with all hardware writes disabled, its own state PVC and ConfigMap, and the new internal Homebridge service. Real SMART and cupboard reads work from the deployed pod. The existing Flux-managed manual deployment remains unchanged. Two ten-minute observation intervals passed: 41 samples from 01:35–01:55 UTC, cupboard 24.3–24.5°C, disks 41–43°C, no read/write/state errors or pod restarts. Requested PWM changed from 38 to 57 after the startup hold; actual PWM stayed 38 on both fans and no hardware change was recorded. Live control and the experiment have not started.

The instructions below preserve the original implementation checklist. Superseding decisions: use cupboard accessory `0xa4c1384342a40d40`, port 30584, internal service `homebridge-cupboard`, and `cupboard-service.yaml`, replacing the AirTouch examples. Dry-run previews decisions against real PWM on every sample rather than simulating fan movement; it never records a simulated hardware success. Standby is displayed explicitly without a temperature instead of displaying an old temperature. The hold starts after write completion, with a conservative 40-second allowance persisted before actuation for crashes during the two SSH calls.

App worktree: `/Users/dvydra/src/dvydra/unas-fan-controller/.amp/worktrees/ambient-fan-curve`. Deployment worktree: `/Users/dvydra/src/dvydra/k3s-config/.amp/worktrees/unas-ambient-control`. Source changes remain uncommitted; the deployed source snapshot is preserved under `.amp/in/artifacts/deployed-source.tar.gz`. Prepared production manifests pin the published digest but have not been applied. Promoting the Flux-managed deployment requires merging the configuration through the normal approval path. Live automatic writes require separate approval after observation. The requested multi-day experiment remains pending baseline measurement and final speed-band confirmation; see the spec's experiment section.

**Architecture:** A server-side controller owns mode, observations, timing, and serialized fan writes. A pure policy chooses requested PWM; bounded Homebridge and SSH readers provide observations. The existing Go HTTP service and embedded page expose controller state and manual override.

**Tech Stack:** Existing Go standard library, golang.org/x/crypto/ssh, embedded HTML/JavaScript, a Python 3 SMART helper on the NAS, and Kubernetes manifests.

**Spec:** `docs/superpowers/specs/2026-10-26-ambient-fan-control-design.md`

## Global Constraints

- Keep ordinary automatic changes at least ten minutes apart; sample every 30 seconds.
- Target hottest-disk temperature 35–40°C. Increase ordinary PWM by 16 above 40°C; decrease by at most 8 below 35°C.
- Ambient minimum: 38 at 20°C and below, 80 at 30°C, 128 at 35°C and above; interpolate and round upward.
- Emergency at 50°C requests PWM 255 immediately. Required readings unavailable for 90 seconds request at least 167, never lowering a higher speed. No valid startup readings means immediate fallback in auto mode.
- Manual-only configurations remain compatible. Auto control requires explicit configuration and remains disabled on the live NAS until separately approved.
- Preserve existing visual style. No new frontend framework or Go dependencies.
- Do not modify unrelated worktrees, install NAS permissions, deploy, push, or publish images without authorization. Leave local implementation uncommitted unless asked to commit.
- No browser request, test, or local preview may write live fan speed.

## Review Focus

1. A hot disk must override an ambient failure: 50°C plus missing ambient requests 255, not 167. Cover in Task 1.
2. Mixed SMART exit flags can include usable data or an actual command failure; never interpret every nonzero status as standby. Cover in Task 2.
3. One missing disk must not silently disappear from the maximum while the others look cool. Cover inventory validation in Task 2 and fallback in Task 3.
4. A concurrent manual request or restart must not permit an old automatic decision to write afterward. Cover serialization and restart timing in Task 3.
5. A pending slider debounce must not switch the app back to manual after the user selects auto. Cancel it and exercise the interaction in Task 4.

## Files and ownership

Keep package `main`; do not reorganize unrelated existing code.

| File | Responsibility |
|---|---|
| `policy.go`, `policy_test.go` | Pure PWM and hold-time decision |
| `readings.go`, `readings_test.go` | Homebridge discovery, SMART payload parsing and validation |
| `nas/disk-temperatures.py`, `nas/test_disk_temperatures.py`, `nas/fancontrol-temperatures.sudoers` | Fixed-purpose privileged SMART reader and fixture tests |
| `controller.go`, `controller_test.go` | Sampling, serialized decisions/writes, mode, persistence, and dry-run behavior |
| `main.go`, `main_test.go` | Configuration, bounded SSH, HTTP integration and contracts |
| `index.html` | Existing UI plus mode, readings, timing, and error status |
| `config.example.json`, `README.md`, `Dockerfile` | Configuration, usage, limitations and package build |
| Separate k3s-config worktree: `apps/unas-fan-controller/{configmap,deployment,kustomization}.yaml`, `state-pvc.yaml` | Opt-in configuration and durable controller state |

## Task 1: Specify the control policy with deterministic tests

**Interfaces:** Produce the following package-level types and function for Task 3. `Current` is the higher of both verified PWM readbacks, so fallback never reduces the faster fan.

```go
type PolicyInput struct {
    Now, NextOrdinaryAt time.Time
    Current int
    Ambient, Hottest float64
    AmbientValid, DiskValid bool
}
type Decision struct {
    PWM int
    Reason string
    Safety bool
}
func Decide(in PolicyInput) Decision
```

- [ ] Write `policy_test.go` first. Use independent expected values, including this hold boundary:

```go
func TestOrdinaryHoldBoundary(t *testing.T) {
    due := time.Unix(1000, 0)
    in := PolicyInput{Current: 80, Ambient: 25, Hottest: 41,
        AmbientValid: true, DiskValid: true, NextOrdinaryAt: due}
    in.Now = due.Add(-time.Nanosecond)
    if got := Decide(in).PWM; got != 80 { t.Fatalf("before boundary: %d", got) }
    in.Now = due
    if got := Decide(in).PWM; got != 96 { t.Fatalf("at boundary: %d", got) }
}
```

- [ ] Add table cases: 34.9/35/40/40.1/49.9/50°C; ambient 19/20/25/30/32.5/35/36°C; PWM clamping at 38 and 255; unknown inputs at current 200 must not reduce it; 50°C with invalid ambient must return 255. Before the hold expires, ordinary ambient increases must also wait. Safety never requests a reduction.
- [ ] Run `go test -run 'TestOrdinaryHoldBoundary|TestPolicy' .` and confirm failures before implementation.
- [ ] Implement the policy in this order: valid hot disk emergency, invalid required input fallback, hold period, disk adjustment, ambient floor, final bounds. For ambient interpolation use `math.Ceil`; 25°C gives 59 and 32.5°C gives 104. Keep stale-sample management out of this pure function.
- [ ] Rerun targeted tests and `go test ./...`. No hardware I/O is permitted in policy tests.

## Task 2: Read real temperatures without granting general root access

**Interfaces:** Produce these types and readers. Null temperatures remain distinguishable from zero; timestamps represent successful local reads, not physical sensor transmission times.

```go
type DiskReading struct {
    Device string `json:"device"`
    Temperature *float64 `json:"temperature,omitempty"`
    Standby bool `json:"standby"`
    Error string `json:"error,omitempty"`
}
func ReadAmbient(ctx context.Context, client *http.Client, baseURL, name string) (float64, error)
func ParseDisks(data []byte, expected []string) ([]DiskReading, error)
```

- [ ] Add `httptest.Server` fixtures to `readings_test.go` with accessories reordered and numeric IDs changed. Resolve `living` by accessory-information Name and Current Temperature on its heater/cooler or temperature service. Accept short and full HAP UUID forms. A duplicate `living`, missing characteristic, HTTP failure, invalid JSON, nonfinite number, or value outside -20–60°C must return an error. Verify only GET requests occur.
- [ ] Define the helper payload and test parsing with unequal temperatures:

```json
{"disks":[{"device":"/dev/sda","temperature":33,"standby":false},{"device":"/dev/sdb","temperature":47,"standby":false},{"device":"/dev/sdc","standby":true}]}
```

Test expected inventory mismatch, duplicate entries, explicit read errors, zero valid active disks, and disk temperatures outside 0–80°C. Standby is not a missing device and its last-known displayed temperature is never a current sample.

- [ ] Run `go test -run 'TestReadAmbient|TestParseDisks' .` and observe failures; implement readers with 5-second HTTP timeout, bounded response size, no redirect to other hosts, and strict validation.
- [ ] Write Python fixture tests before the helper. Mock `subprocess.run`, never real devices, and feed SMART JSON containing `temperature.current`, standby status, a missing device, and command failures. Reject arguments and reject ambiguous standby detection. Use SMART status bits and explicit power-state evidence; do not treat health-warning bits alone as missing temperature.
- [ ] Implement the helper with a fixed tuple `/dev/sda` through `/dev/sde`, absolute `/usr/sbin/smartctl`, `-j -n standby -A`, a per-device timeout, JSON output, and no shell. Start with an isolated Python interpreter shebang (`#!/usr/bin/python3 -I`) and validate Python availability on the NAS read-only before installation instructions are finalized.
- [ ] Add the exact sudoers command, with the empty argument string enforcing no arguments:

```sudoers
fancontrol ALL=(root) NOPASSWD: /usr/local/sbin/unas-disk-temperatures ""
```

The helper and every parent directory must be root-owned and not writable by `fancontrol`. Do not install it yet. Test with `python3 -m unittest discover -s nas -p 'test_*.py'`; syntax-check the helper without leaving bytecode in the repository. Validate sudoers with `visudo -cf` in a disposable Linux environment if available, otherwise report that validation gap.

## Task 3: Add the server-side controller, persistent mode, and HTTP contracts

**Interfaces:** Consume Task 1 policy and Task 2 readers. Keep the controller's I/O injectable with function fields rather than a general plugin framework:

```go
type ControllerIO struct {
    Ambient func(context.Context) (float64, error)
    Disks func(context.Context) ([]DiskReading, error)
    ReadPWM func(context.Context) ([2]int, error)
    WritePWM func(context.Context, int) error
}
func (c *Controller) Sample(ctx context.Context, now time.Time)
func (c *Controller) SetMode(ctx context.Context, mode string, now time.Time) error
func (c *Controller) SetManual(ctx context.Context, pwm int, now time.Time) error
func (c *Controller) Status() ControllerStatus
```

`ControllerStatus` contains mode, dry-run flag, timestamped ambient and disks, both observed PWM values, desired PWM, reason, last successful change, next ordinary change, and read/write/persistence errors. JSON fields use snake_case. A nullable reading is unavailable, never rendered as 0°C. All methods synchronize through the controller, with one serialized sampling/mutation path and a short status-read lock.

- [ ] Write fake-I/O controller tests using explicit times and a temporary state directory. Example sequence: sample valid readings at time zero with PWM 80; request 96 for a 41°C disk at ten minutes; confirm no write at 9:59; verify readback and persisted timestamp at 10:00; reconstruct controller and confirm no ordinary write before 20:00.
- [ ] Add tests for a 90-second missing-input boundary, hot disk during ambient outage, first-sample failure, standby transitions, partial inventory, and recovery. Cache the last good reading per source; a missing disk must age independently. An entire failed sample must never refresh prior timestamps. In failure grace periods, do not reduce cooling based on old data.
- [ ] Add a blocking fake writer test for manual/automatic ordering, a partial write/readback mismatch, a manual-mode restart, state-file failure, and a failed safety request retried on the next tick. Use channels rather than sleeps. Only confirmed changes update the successful-write timestamp; uncertain or partial writes conservatively postpone ordinary changes by ten minutes.
- [ ] Implement `controller.go` with a 30-second ticker and explicit cancellation, a ten-minute startup/auto-entry hold, last-good sample caches, and atomic state persistence through a temporary file plus rename. Mode changes must persist before reporting success. On persistence failure, inhibit automatic reductions, retain a safe in-memory hold, and report the error. Never reset a hold simply because a sampling tick repeats the same PWM.
- [ ] Persist mode and successful-change time. On restart use the later of the persisted hold and a new startup hold if write outcome is uncertain; do not infer that an invalid future timestamp means cooling can be reduced. Keep mode configuration separate from whether auto integration is available.
- [ ] Extend `Config` with optional controller configuration: Homebridge URL, accessory name, expected disks, state path, initial mode, and dry-run flag. Defaults preserve manual-only behavior. Require complete integration settings and writable persistent state before auto can be enabled. Add example configuration showing initial manual mode and dry-run enabled.
- [ ] Add dry-run behavior that samples and reports policy decisions without calling `WritePWM`. The ten-minute simulated decisions have their own in-memory timing; never record them as successful hardware writes. Mark dry-run clearly in status and UI. Existing manual hardware writes are disabled in dry-run so local validation cannot accidentally actuate fans.
- [ ] Bound the entire SSH operation, including handshake and command completion: use a context-aware TCP dial, a connection deadline, and connection close on cancellation. Keep commands fixed. The disk reader runs `sudo -n /usr/local/sbin/unas-disk-temperatures`; PWM operations use integer validation and fixed sysfs paths. Read back both channels after writes and preserve existing `/api/fan` response fields.
- [ ] Add `GET /api/controller` and `POST /api/mode` (`{"mode":"auto"}` or `{"mode":"manual"}`). Invalid modes, missing/null/fractional/out-of-range speed values, oversized bodies, and trailing JSON are rejected without hardware writes. Existing `POST /api/fan` becomes a serialized manual action; `/api/sensors` stays compatible. Return errors when writes or persistence fail.
- [ ] Run failing HTTP contract tests before wiring the handlers, then `go test -race ./...` and `go vet ./...`. Log successful changes with old/new PWM, reason and temperatures; do not log credentials or entire configuration.

## Task 4: Expose status, verify the app, and prepare deployment safely

**Interfaces:** Use `/api/controller` status and `/api/mode` from Task 3; retain the existing manual `/api/fan` contract.

- [ ] Extend `index.html` in its existing style with auto/manual selection, dry-run indication, ambient and per-disk readings, observed/requested PWM, next-change time, and persistent error/fallback text. Use `textContent` for sensor names and errors. Show manual mode as having no automatic safety control.
- [ ] Cancel `debounceTimer` when switching modes. Disable conflicting controls while a mode request is pending; refresh authoritative status after completion. Poll controller status without sending speed changes, and do not move the slider while the user is editing it. Show failed status fetches without relabeling old readings as fresh.
- [ ] Run a local fixture server for default manual, auto-hold, fallback, dry-run, and error states. Use browser network assertions to verify no POST on load or polling, one debounced manual action, and no delayed manual action after selecting auto. Capture and inspect each affected state at desktop and narrow widths. Keep fixture tooling under excluded `.amp/in/` and remove temporary scripts afterward; preserve final inspected artifacts for the response.
- [ ] Change the Dockerfile build target from `main.go` to `.` and README local commands from `go run main.go` to `go run .`. Verify `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/unas-fan-controller-check .`, then remove that temporary binary. If Docker is available, build locally without pushing.
- [ ] Update README with configuration, mode/restart behavior, sensor freshness limitations, no watchdog guarantee, manual rollback, and exact staged helper-install commands. Present privileged installation as an approval-required step; never execute it during local verification.
- [ ] Before deployment edits, check ownership and create a fresh k3s-config worktree from fetched `origin/main`. Inspect its current guidance and storage conventions. Add a small PVC using the existing storage class, mount it at `/state`, and extend the init configuration generator with internal Homebridge URL `http://homebridge-airtouch.homebridge.svc.cluster.local:56902`, accessory `living`, the five-disk inventory, `/state/controller.json`, manual mode, and dry-run enabled. The existing service exposes only port 8581, so add `apps/homebridge/airtouch-service.yaml` defining a separate ClusterIP service named `homebridge-airtouch` exposing child-bridge port 56902 to the homebridge pods, and register it in `apps/homebridge/kustomization.yaml`; no ingress or external load balancer. Check the selector against the existing Homebridge service.
- [ ] Render both affected kustomizations and validate resource references locally. Do not choose or deploy a nonexistent image tag: retain the current image until an authorized build has produced an immutable image. The manifest's final image pin and rollout are explicitly delivery-blocked until publication approval.
- [ ] Finish with `gofmt`, reread modified files, `go test -race ./...`, `go vet ./...`, helper tests, manifest rendering, and inspected UI captures. Use hunk-diff for presenting changes. Report actual local delivery state, no unqualified deployed/completed claim.

## Approval-required rollout, after implementation

Do not execute these steps as part of local implementation. Obtain specific permission to install the restricted NAS helper/sudoers entry, publish the application image, and deploy manifests. After installation, verify read-only SMART access as `fancontrol`; after deployment, verify ambient reads from the actual app network. Observe dry-run output for at least two ten-minute intervals, then request approval to enable live auto control. Keep the existing manual PWM-permissions service untouched.

## Plan self-review

Coverage maps directly to the spec: policy and timing in Task 1; Homebridge/SMART and permissions in Task 2; server lifecycle, state, failures and HTTP in Task 3; UI, packaging, documentation and deployment preparation in Task 4. Tests cover each Review Focus case. Existing `main.go`-only builds and Homebridge's service-port mismatch are explicitly addressed. Numeric control defaults match the approved spec. No independent NAS watchdog, HVAC control, unrelated refactor, automatic deployment, or additional agent is included.
