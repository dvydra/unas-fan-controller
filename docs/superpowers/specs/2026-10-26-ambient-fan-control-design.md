# Ambient-aware UNAS fan control

## Goal and agreed constraints

Keep the hottest NAS disk around 35–40°C while avoiding unnecessary fan noise. Read cupboard temperature from accessory `0xa4c1384342a40d40` in Homebridge's Zigbee child bridge on port 30584. The user paired and placed this sensor during implementation, replacing AirTouch Living as the control input. Disk temperature takes priority. Ordinary automatic fan-speed changes must be at least ten minutes apart. Overheating or unavailable readings may increase cooling immediately; reductions always wait. Manual changes remain immediate.

This extends the existing Go fan app, not the extraction fan or HVAC controls. No deployment, NAS permission change, or live automatic fan control is authorized by approval of this design alone.

## Observed integration points

- The app currently exposes manual PWM control through `/api/fan`, writes both `pwm1` and `pwm2`, and serves raw `sensors` output through `/api/sensors`. It has no background controller.
- Homebridge's AirTouch child bridge answered a read-only `/accessories` request on port 56902. Accessory `living` has a Current Temperature characteristic; the live value was 17.9°C. Saved accessory files contained an older value and must not supply control input.
- Match the configured accessory name and service/characteristic types during discovery, not observed numeric accessory IDs. Reject missing or ambiguous matches. Use read-only requests and never modify HVAC characteristics.
- SMART reported temperatures for five disks, `/dev/sda` through `/dev/sde`. The app's `fancontrol` account cannot open those devices. A narrowly restricted, root-owned helper is needed, rather than running the app as root.
- PWM 38 produced about 700 rpm; PWM 167 produced about 4,250 rpm. These are observations, not a calibrated RPM curve.

## Proposed initial control policy

The following numeric policy is a starting point for review and later observation, not a claim of calibrated cooling performance.

Sample every 30 seconds with bounded network and command timeouts. Use the hottest valid disk reading. Preserve individual disk values, sample times, errors, and the last successful write in controller status.

At each eligible ten-minute adjustment:

- Above 40°C, increase PWM by 16.
- From 35°C through 40°C, hold PWM unless the ambient minimum is higher.
- Below 35°C, decrease PWM by at most 8, never below the ambient minimum.
- Clamp ordinary automatic output to 38–255.

The ambient minimum is PWM 38 at or below 20°C, rising linearly to PWM 80 at 30°C and PWM 128 at 35°C, then remaining at 128. Interpolate between those points. Cupboard temperature may raise the minimum; it never suppresses cooling requested by a hot disk. AirTouch Living remains a separate room reference for the later experiment.

At a hottest-disk temperature of 50°C or higher, immediately request PWM 255. This is a proposed conservative emergency threshold, not a manufacturer limit. Missing or invalid temperature inputs request at least PWM 167 without lowering a higher current speed. A first transient read failure retains the last valid sample; after 90 seconds without a valid required sample, enter fallback. At startup without usable samples, fallback applies immediately. Recovery requires valid inputs and obeys the ten-minute reduction delay.

The hold period starts at the last successful speed change, including a safety increase or manual change. Failed writes do not update successful-write state. Serialize decisions and writes so a stale automatic decision cannot overwrite a manual action. Verify both PWM values after a write; a partial write is an error, not a reported success. Retry failed safety writes on subsequent sampling ticks, without claiming that cooling increased.

## Modes and restart behavior

Existing configurations remain manual-only. An explicit configured auto mode enables the loop. Selecting manual mode suspends curve writes; switching to auto starts a fresh observation period and never immediately reduces cooling. The existing slider selects manual mode when used.

Persist mode and the last successful change time in a small state file on a durable writable volume. On restart, read the actual PWM values and honor the remaining hold time. If state is missing or invalid, do not make an ordinary change for ten minutes. Safety increases remain available in auto mode. Manual mode is explicitly manual and the UI must not imply that automatic safety protection remains active there.

## Temperature access and failure handling

Provide a root-owned, fixed-purpose NAS helper and a narrowly scoped sudoers entry allowing `fancontrol` to invoke that helper without arguments. It must only read the configured disks' SMART temperatures and emit structured results. No caller-supplied shell command, device path, or arbitrary smartctl options are accepted. Keep its code and installation instructions in the app repository. Installation requires separate approval.

Use `smartctl -n standby` so sampling does not wake sleeping disks. A sleeping disk is an explicit state, not a zero-degree reading or a missing disk. Retain its last reading for display but exclude that old value from the current maximum. Missing devices, malformed output, and permission errors are failures. If no disk has a current usable temperature, use fallback rather than assuming the NAS is cold. Validate temperature values as finite and plausible before using them.

Homebridge request success proves that Homebridge answered, not that the physical Zigbee sensor recently transmitted. An unchanged temperature alone is not proof of a stale sensor. Report this limitation rather than inventing a freshness timestamp.

If the app crashes or loses SSH access, it cannot enforce fallback and the NAS may retain its last manual PWM. This release must state that limitation clearly; an independent NAS watchdog is outside this design and must not be implied by the word fallback.

## App and deployment changes

Keep the existing sensor and fan routes compatible. Add a controller-status endpoint and a mode endpoint. The page shows mode, live ambient temperature, per-disk temperatures, current and requested PWM, next eligible ordinary change, and any fallback or write error. Preserve the existing visual style and slider.

Keep sampling and control in the server so closing the browser does not stop it. Separate the control policy from SSH and Homebridge I/O for deterministic tests. Use the existing Go HTTP stack and embedded page; no new service or frontend framework is needed.

Prepare deployment configuration in an isolated k3s-config worktree after implementation: internal Homebridge endpoint, sensor identity, fixed disk inventory, state volume, and an immutable application image reference. Verify Homebridge access from the application network before enabling auto mode. Do not expose its unauthenticated child-bridge port publicly. Leave the existing PWM-permissions service unchanged.

## Verification and rollout

Tests must exercise asymmetric disk values, exact 35/40/50°C boundaries, the ten-minute boundary, ambient interpolation, invalid and stale inputs, sleeping and missing disks, recovery, partial writes, concurrent manual changes, and restart hold behavior. Use controlled time and fake I/O, never live hardware, for those tests.

Run Go tests with the race detector and vet. Test HTTP contracts and render the manual, auto, and fallback UI states with fixture readings; inspect captures. Validate the helper against sanitized SMART fixtures and check its syntax. Real sensor reads may be validated without writing fan speed.

Deploy only after approval. Initially observe proposed outputs without hardware writes, compare them with actual temperatures, then explicitly enable automatic mode. Check at least two ordinary control intervals and confirm the ten-minute rule from logs. Reverting to manual mode stops automatic changes. No claims of stable disk temperatures or reboot survival without observing those conditions.

## Requested multi-day experiment, pending rollout

The user requested controlled testing after the app is ready. Do not interpret this as a simple ambient × fan-speed equation: cupboard and disk temperatures respond with delays, and disk activity adds heat. Record room temperature from AirTouch Living, cupboard temperature, each disk temperature, actual PWM and RPM, disk activity, timestamps, and interventions every minute.

Proposed initial design: establish a baseline, then compare PWM 48, 56 and 64 in balanced two-hour blocks over 72 hours, with no changes more often than ten minutes. Confirm this band against initial temperatures before starting. Abort the experimental schedule at a disk temperature of 45°C, missing required readings, or a write/readback failure; return to the automatic safety policy. Retain the 50°C emergency rule. Do not run a second independent fan writer alongside the controller.

Fit temperature rise above room temperature against PWM, workload and lagged readings. Report settled portions separately from transitions, uncertainty, and the observed range; do not extrapolate beyond tested fan speeds or claim causality from room-temperature correlation alone. The experiment runner and durable telemetry collection are not implemented or started yet. They depend on rollout, baseline observations, and confirming the speed band and abort settings.
