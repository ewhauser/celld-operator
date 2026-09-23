#!/usr/bin/env python3
"""Sample shared-store/proxy cost during a density run, without probing apps."""
import argparse
import json
from pathlib import Path
import time
from run import Experiment, cmd

p = argparse.ArgumentParser(description=__doc__)
p.add_argument("--run", required=True)
p.add_argument("--output", required=True)
args = p.parse_args()
client = Experiment.__new__(Experiment)
client.socket = json.loads(cmd("docker", "context", "inspect"))[0]["Endpoints"]["docker"]["Host"].removeprefix("unix://")
samples = []
started = time.monotonic()
while time.monotonic() - started < 1800:
    containers = [c for c in client.api("/containers/json") if c.get("Labels", {}).get("celld.preview-density") == args.run]
    if not containers:
        break
    sample = {"elapsedSeconds": time.monotonic() - started, "containerCount": len(containers), "services": []}
    for c in containers:
        name = c["Names"][0].lstrip("/")
        if name.endswith(("-store", "-proxy")):
            try:
                sample["services"].append(client.stats(name))
            except Exception as e:
                sample.setdefault("errors", []).append(str(e))
    samples.append(sample)
    Path(args.output).write_text(json.dumps({"run": args.run, "samples": samples}, indent=2) + "\n")
    time.sleep(10)
