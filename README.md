# UNAS Fan & Temperature Controller

A simple, secure, and responsive web application to safely monitor temperatures and control fan speeds on UNAS devices remotely via SSH.

This lightweight Go application provides a beautiful Material Design web interface for your NAS, allowing you to check system sensor data and adjust fan speeds seamlessly without directly exposing root credentials to potential attackers.

## Features

- **Real-time Monitoring**: Automatically fetches output from the `sensors` command directly from the NAS.
- **Fan Adjustments**: A simple UI slider allows for dynamically adjusting standard fan pulse-width modulation (`pwm1` and `pwm2`) variables.
- **Defensive API**: Pre-calculated commands are sent over the SSH connection; arbitrary command execution from the web interface is not possible.
- **Extremely Lightweight**: Built purely in Go using the standard library (and x/crypto/ssh for connectivity), with no other heavy dependencies or external front-end framework files required (embedded HTML).
- **Dark Mode UI**: Responsive and modern single-page frontend.

## Prerequisites

Before setting up the UNAS Fan Controller, ensure you have:

1. **A UNAS Device / Linux NAS**: Your system must support `lm-sensors` for temperature readings (via the `sensors` command) and expose fan control via `/sys/class/hwmon/hwmon0/pwm1` and `pwm2`.
2. **SSH Access**: You need SSH access to the NAS to execute these commands (ideally using SSH keys rather than a password).
3. **Go 1.25.5+**: To build the application yourself (if installing from source).

## Installation & Running Locally

1. Clone this repository:
   ```bash
   git clone https://github.com/yourusername/unas-fan-controller.git
   cd unas-fan-controller
   ```
2. Copy the example configuration file:
   ```bash
   cp config.example.json config.json
   ```
3. Edit the `config.json` file to match your environment (see **Configuration** below).
4. Start the application:
   ```bash
   go run .
   ```
   *Note: If you want to deploy the binary on a server, run `go build -o unas-fan-controller` instead of `go run`, and launch the generated executable.*

5. Open your browser to `http://localhost:8080`.

## Configuration

By default, the application looks for a `config.json` file in its working directory. 

### Configuration File (`config.json`)

```json
{
  "host": "192.168.1.100",
  "port": 22,
  "user": "root",
  "key_file": "/home/youruser/.ssh/id_rsa"
}
```

- `host`: The IP address or hostname of your NAS.
- `port`: The SSH port (usually `22`).
- `user`: The SSH user on the NAS (must have permission to read/write the `/sys/.../pwm*` files and execute `sensors`).
- `key_file`: Absolute path to the private SSH key for authentication. Passwords are not supported for security reasons; you must use an SSH key.

### Environment Variables

You can override certain settings via environment variables:

- **Config Path**: Override the default `config.json` path.
  ```bash
  CONFIG_PATH=/etc/unas-fan-controller/config.json ./unas-fan-controller
  ```
- **Port**: Change the port the web UI listens on (default is `8080`).
  ```bash
  PORT=3000 ./unas-fan-controller
  ```

## Security Considerations

Since this application can execute adjustments on your NAS hardware, it must be deployed securely:

1. **Network Segregation**: The application web UI is not meant to be exposed to the open internet. Access it only via a secure VPN, local network, or put it behind a rigorous reverse proxy with strong authentication (like Authelia or an OAuth proxy).
2. **SSH Keys**: Passwords are intentionally not supported in `config.json`. You must generate an SSH key pair specifically for this application and provide the `key_file` path. 
3. **Non-Root Execution (Highly Recommended)**: The controller should not connect as `root`. You can configure your NAS to allow a standard user to read temperatures and control the fans safely via **udev rules**.

### Configuring a Non-Root User (udev approach)

By default, the `pwm*` files in `/sys/class/hwmon/...` are only writable by `root`. To allow a dedicated user (e.g., `fanuser`) to control them:

1. Create a system group (e.g., `fancontrol`) and add your SSH user to it:
   ```bash
   sudo groupadd fancontrol
   sudo usermod -aG fancontrol fanuser
   ```

2. Create a udev rule to automatically assign group ownership and write permissions to the PWM files on boot. Create a file like `/etc/udev/rules.d/99-fancontrol.rules`:
   ```udev
   # Adjust the hwmon path if necessary; this example targets hwmon0
   ACTION=="add", SUBSYSTEM=="hwmon", KERNEL=="hwmon0", RUN+="/bin/chgrp fancontrol /sys/class/hwmon/hwmon0/pwm1 /sys/class/hwmon/hwmon0/pwm2", RUN+="/bin/chmod 664 /sys/class/hwmon/hwmon0/pwm1 /sys/class/hwmon/hwmon0/pwm2"
   ```

3. Reload udev rules (or reboot the NAS):
   ```bash
   sudo udevadm control --reload-rules && sudo udevadm trigger
   ```

Now, update your `config.json` to use `"user": "fanuser"` instead of `root`. The `sensors` command requires no special privileges, and the user will now have secure, restricted write access to just the fan control files.

## License

This project is licensed under the MIT License - see the LICENSE file for details.

## Automatic cooling

The optional `controller` section in `config.example.json` reads the cupboard sensor `0xa4c1384342a40d40` from Homebridge's Zigbee child bridge on port 30584 and SMART temperatures from five fixed NAS disks. Omit the section for the original manual-only behavior. Create the state directory before startup and mount it on persistent storage. Keep the Homebridge endpoint internal; it has no authentication.

Initial configuration is manual with dry-run enabled. Select Automatic to preview decisions. With `dry_run: true`, neither automatic nor manual requests can write hardware. Dry-run evaluates against actual PWM every sample; it does not simulate the thermal response or future PWM steps. Turning dry-run off requires a configuration change and restart. Persisted mode overrides the configured initial mode.

The controller samples every 30 seconds. Ordinary changes wait at least ten minutes after the previous write completes, including across restarts. Above 40°C it adds 16 PWM; below 35°C it subtracts at most 8. The cupboard minimum rises from 38 PWM at 20°C to 80 at 30°C and 128 at 35°C. These are initial settings, not a calibrated thermal model.

In Automatic mode, a disk at 50°C requests 255 immediately. Required readings missing for 90 seconds request at least 167 without reducing a faster fan. No usable startup readings trigger fallback immediately. Sleeping disks are not awakened; if all disks sleep, fallback applies. Reductions wait for fresh readings and the ten-minute hold. Both fan values are read back, and partial writes remain errors. Failed or interrupted writes reserve an extra 40 seconds for the bounded write/readback calls before ordinary changes can resume.

Selecting Manual stops automatic changes and automatic safety protection. Moving the slider also selects Manual. Manual changes remain immediate. The page shows sample times and read/write errors; `GET /api/controller` exposes the same status. `POST /api/mode` accepts `{"mode":"auto"}` or `{"mode":"manual"}`.

### Limitations

- A successful Homebridge request does not prove the battery sensor recently transmitted. The displayed timestamp is the API read time, not the Zigbee report time. A disconnected sensor can remain cached in Homebridge.
- If the app stops or loses SSH access, it cannot raise fan speed. There is no independent NAS watchdog.
- State persistence uses a local file. Losing the volume starts a new ten-minute hold; safety increases remain available in Automatic mode.
- SSH retains the existing unverified host-key policy. Keep the application behind authenticated access on the trusted network.

### Restricted SMART helper installation

Approval is required before installing these files on the NAS. The helper has a fixed disk list and accepts no arguments; the application account receives no general root shell or arbitrary SMART access. The following commands are for this installation's checked-out worktree and NAS. They refuse to replace existing files. No token environment variables are needed; root SSH key access is required.

```bash
cd /Users/dvydra/src/dvydra/unas-fan-controller/.amp/worktrees/ambient-fan-curve
ssh root@192.168.1.234 'test ! -e /usr/local/sbin/unas-disk-temperatures && test ! -e /etc/sudoers.d/fancontrol-temperatures && install -d -m 0700 /root/unas-fan-staging'
scp nas/disk-temperatures.py nas/fancontrol-temperatures.sudoers root@192.168.1.234:/root/unas-fan-staging/
ssh root@192.168.1.234 'visudo -cf /root/unas-fan-staging/fancontrol-temperatures.sudoers && install -o root -g root -m 0755 /root/unas-fan-staging/disk-temperatures.py /usr/local/sbin/unas-disk-temperatures && install -o root -g root -m 0440 /root/unas-fan-staging/fancontrol-temperatures.sudoers /etc/sudoers.d/fancontrol-temperatures && sudo -u fancontrol sudo -n /usr/local/sbin/unas-disk-temperatures'
```

The helper and its parent directories must remain root-owned and not writable by `fancontrol`. Keep the existing PWM permissions service unchanged. Publish an immutable app image and pin that image before deploying the prepared manifests. Observe dry-run for two ten-minute intervals before enabling writes; installation alone does not enable automatic control.
