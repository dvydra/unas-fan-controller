import importlib.util
import json
import pathlib
import subprocess
import unittest
from unittest.mock import patch

path = pathlib.Path(__file__).with_name("disk-temperatures.py")
spec = importlib.util.spec_from_file_location("temperatures", path)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

class ReadDiskTests(unittest.TestCase):
    def test_temperature_and_health_warning(self):
        for status in (0, 8, 64):
            result = subprocess.CompletedProcess([], status, json.dumps({"temperature": {"current": 39}}))
            with patch.object(module.subprocess, "run", return_value=result):
                self.assertEqual(module.read_disk("/dev/sda")["temperature"], 39)

    def test_standby_not_permission_failure(self):
        for payload, status, standby in [
            ({"power_mode": {"string": "STANDBY"}}, 2, True),
            ({"smartctl": {"messages": [{"string": "open failed"}]}}, 2, False),
            ({"temperature": {"current": 99}}, 0, False),
            ({"temperature": {"current": True}}, 0, False),
        ]:
            with patch.object(module.subprocess, "run", return_value=subprocess.CompletedProcess([],status,json.dumps(payload))):
                result = module.read_disk("/dev/sda")
                self.assertEqual(result.get("standby",False), standby)
                if not standby: self.assertIn("error", result)

    def test_timeout(self):
        with patch.object(module.subprocess,"run",side_effect=subprocess.TimeoutExpired("smartctl",3)):
            self.assertIn("error",module.read_disk("/dev/sda"))

    def test_arguments_rejected(self):
        with patch.object(module.sys,"argv",["helper","/dev/other"]):
            self.assertEqual(module.main(),2)
