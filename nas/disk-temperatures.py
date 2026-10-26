#!/usr/bin/python3 -I
"""Read only the five UNAS data disks. Install root-owned, with no arguments."""
import json
import math
import subprocess
import sys

DISKS = ("/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/sdd", "/dev/sde")

def read_disk(device):
    result = {"device": device, "standby": False}
    try:
        process = subprocess.run(
            ["/usr/sbin/smartctl", "-j", "-n", "standby", "-A", device],
            capture_output=True, text=True, timeout=3,
            env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL": "C"},
        )
        data = json.loads(process.stdout)
        power = data.get("power_mode", {}).get("string", "").upper()
        if process.returncode == 2 and power in ("STANDBY", "SLEEP"):
            result["standby"] = True
            return result
        if process.returncode < 0 or process.returncode & 7:
            raise ValueError("SMART command failed")
        temperature = data.get("temperature", {}).get("current")
        if type(temperature) not in (int, float) or not math.isfinite(temperature) or not 0 <= temperature <= 80:
            raise ValueError("missing or invalid SMART temperature")
        result["temperature"] = temperature
    except (OSError, ValueError, TypeError, AttributeError, subprocess.TimeoutExpired) as error:
        result["error"] = str(error)
    return result

def main():
    if len(sys.argv) != 1:
        print("This helper accepts no arguments", file=sys.stderr)
        return 2
    print(json.dumps({"disks": [read_disk(device) for device in DISKS]}, allow_nan=False))
    return 0

if __name__ == "__main__":
    sys.exit(main())
