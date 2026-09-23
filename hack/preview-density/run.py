#!/usr/bin/env python3
"""Disposable local fleet-density experiment. Requires Docker and built launcher."""
import argparse
import concurrent.futures
import hashlib
import http.client
import json
import os
from pathlib import Path
import socket
import statistics
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[2]
IMAGE = "ghcr.io/ewhauser/celld@sha256:4b9eb5656054580e7dd5ed2bbd9ee8b641ecd60c317437e9be63f4e3ae333f29"
MINIO = "quay.io/minio/minio@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e"
MC = "quay.io/minio/mc@sha256:a7fe349ef4bd8521fb8497f55c6042871b2ae640607cf99d9bede5e9bdf11727"
NODE = "node@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1"
MIB = 1024 * 1024


def cmd(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT).strip()


def parallel(fn, items, workers=4):
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
        return list(pool.map(fn, items))


def request(url, data=None, method=None):
    try:
        with urllib.request.urlopen(urllib.request.Request(url, data=data, method=method), timeout=20) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=15)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class Experiment:
    def __init__(self, args):
        self.args = args
        self.name = "preview-density-" + uuid.uuid4().hex[:10]
        self.containers = []
        self.network = False
        self.temp = tempfile.TemporaryDirectory(prefix=self.name, dir=ROOT / "bin")
        self.key = Path(self.temp.name) / "key"
        self.key.write_bytes(os.urandom(32))
        self.key.chmod(0o644)
        context = json.loads(cmd("docker", "context", "inspect"))[0]
        self.socket = context["Endpoints"]["docker"]["Host"].removeprefix("unix://")
        self.result = {"run": self.name, "startedUTC": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                       "runtime": IMAGE, "args": vars(args), "phases": []}
        self.result["host"] = self.api("/info")
        # Store only capacity/version fields, not the Docker host's full inventory.
        self.result["host"] = {k: self.result["host"].get(k) for k in ["NCPU", "MemTotal", "Architecture", "KernelVersion", "ServerVersion"]}
        self.result["launcherSHA256"] = hashlib.sha256((ROOT / "bin/preview-density-launcher").read_bytes()).hexdigest()

    def api(self, path):
        c = UnixHTTP(self.socket)
        try:
            c.request("GET", path)
            r = c.getresponse()
            b = r.read()
            if r.status != 200:
                raise RuntimeError(f"Docker {path}: {r.status} {b!r}")
            return json.loads(b)
        finally:
            c.close()

    def save(self):
        p = Path(self.args.output)
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(json.dumps(self.result, indent=2) + "\n")

    def log(self, message):
        print(time.strftime("%H:%M:%S"), message, flush=True)

    def run_container(self, name, arguments):
        # Register before launching so failures also get cleaned up.
        self.containers.append(name)
        return cmd("docker", "run", "-d", "--name", name, "--label", "celld.preview-density=" + self.name,
                   "--network", self.name, *arguments)

    def port(self, name, port):
        return "http://" + cmd("docker", "port", name, str(port) + "/tcp")

    def env(self, prefix):
        return {"AWS_ACCESS_KEY_ID": "density", "AWS_SECRET_ACCESS_KEY": "density-local-only", "AWS_REGION": "us-east-1",
                "AWS_ALLOW_HTTP": "true", "S3_ENDPOINT": "http://store-proxy:9000", "CELLD_BUCKET": "s3://previews/" + prefix}

    @staticmethod
    def env_args(env):
        return [v for k, value in env.items() for v in ("-e", k + "=" + str(value))]

    def setup(self):
        cmd("docker", "network", "create", self.name)
        self.network = True
        self.run_container(self.name + "-store", ["--network-alias", "minio", "--memory", "768m",
            "--tmpfs", "/data:rw,size=512m", "-e", "MINIO_ROOT_USER=density", "-e", "MINIO_ROOT_PASSWORD=density-local-only",
            MINIO, "server", "/data"])
        self.run_container(self.name + "-proxy", ["--network-alias", "store-proxy", "--memory", "128m", "-p", "127.0.0.1::9001",
            "-v", str(ROOT / "hack/preview-density/proxy.mjs") + ":/proxy.mjs:ro", NODE, "node", "/proxy.mjs"])
        self.metrics = self.port(self.name + "-proxy", 9001)
        for _ in range(30):
            try:
                cmd("docker", "run", "--rm", "--network", self.name, "--entrypoint", "/bin/sh", MC, "-c",
                    "mc alias set local http://minio:9000 density density-local-only >/dev/null && mc mb local/previews")
                return
            except subprocess.CalledProcessError:
                time.sleep(1)
        raise RuntimeError("MinIO failed to start")

    def deploy(self, prefix):
        cmd("docker", "run", "--rm", "--network", self.name, *self.env_args(self.env(prefix)),
            "-v", str(ROOT / "hack/preview-density/app") + ":/app:ro", IMAGE, "deploy", "/app")

    def start(self, index, profile, limit, prefix=None):
        name = f"{self.name}-{profile}-{limit}-{index}-" + uuid.uuid4().hex[:4]
        prefix = prefix or name
        began = time.monotonic()
        if prefix == name:
            self.deploy(prefix)
        deployed = time.monotonic()
        env = self.env(prefix) | {"POD_UID": name, "NODE_NAME": "density-host", "CELLD_NODE": name,
            "CELLD_ADVERTISE": name + ":8081", "CELLD_ADDR": "0.0.0.0:8080", "CELLD_INTERNAL_ADDR": "0.0.0.0:8081",
            "CELLD_WATCH": "/work", "CELLD_DURABILITY": "bucket", "CELLD_TTL_MS": "10000", "CELLD_TOKIO_THREADS": "2",
            "CELLD_SHUTDOWN_TOTAL_MS": "20000"}
        if profile in ("small", "quiet"):
            env |= {"CELLD_MAX_STATELESS_ISOLATES": "1", "CELLD_MAX_REQUESTS": "8", "CELLD_MAX_CELL_REQUESTS": "4",
                    "CELLD_MAX_RESIDENT_CELLS": "8", "CELLD_IDLE_EVICT_S": "30", "CELLD_MAX_REQUEST_BODY_BYTES": str(MIB)}
        if profile == "quiet":
            env |= {"CELLD_TTL_MS": "60000", "CELLD_DEPLOY_POLL_S": "300", "CELLD_REBALANCE_INTERVAL_MS": "0"}
        self.run_container(name, ["--memory", f"{limit}m", "--memory-swap", f"{limit}m", "-p", "127.0.0.1::8080",
            "-v", str(ROOT / "bin/preview-density-launcher") + ":/launcher:ro", "-v", str(self.key) + ":/launcher-key/key:ro",
            *self.env_args(env), "--entrypoint", "/launcher", IMAGE])
        url = self.port(name, 8080)
        first_response = None
        while time.monotonic() - deployed < 90:
            try:
                status, body = request(url)
                if status == 200 and json.loads(body).get("version") == "density-v1":
                    if first_response is None:
                        first_response = time.monotonic() - deployed
                    if request(url + "/.well-known/celld/health")[0] == 200:
                        return {"name": name, "prefix": prefix, "url": url, "profile": profile, "limitMiB": limit,
                                "deploySeconds": deployed - began, "firstResponseSeconds": first_response,
                                "readySeconds": time.monotonic() - deployed}
            except (OSError, ValueError):
                pass
            state = self.api("/containers/" + name + "/json")["State"]
            if not state["Running"]:
                raise RuntimeError(f"{name} exited: {state}; {cmd('docker', 'logs', name)[-4000:]}")
            time.sleep(.2)
        raise RuntimeError(f"{name} not ready: {cmd('docker', 'logs', name)[-4000:]}")

    def stats(self, name):
        s = self.api("/containers/" + name + "/stats?stream=false&one-shot=true")
        mem = s.get("memory_stats", {})
        detail = mem.get("stats", {})
        return {"name": name, "usage": mem.get("usage", 0), "working": mem.get("usage", 0) - detail.get("inactive_file", detail.get("total_inactive_file", 0)),
                "anon": detail.get("anon", detail.get("rss", 0)), "cpuNS": s.get("cpu_stats", {}).get("cpu_usage", {}).get("total_usage", 0),
                "pids": s.get("pids_stats", {}).get("current", 0)}

    def counts(self):
        return json.loads(request(self.metrics)[1])

    def measure(self, nodes, label, seconds=None):
        seconds = seconds or self.args.seconds
        self.log(f"Measuring {label}: {len(nodes)} fleets for {seconds}s")
        before = self.counts()
        start = time.monotonic()
        initial = parallel(lambda n: self.stats(n["name"]), nodes)
        samples = [initial]
        health = {}
        while time.monotonic() - start < seconds:
            def probe(n):
                try:
                    return request(n["url"] + "/.well-known/celld/health")[0]
                except OSError:
                    return "connection-error"
            for status in parallel(probe, nodes, 8):
                health[str(status)] = health.get(str(status), 0) + 1
            if len(samples) == 1 or time.monotonic() - sampled > 10:
                samples.append(parallel(lambda n: self.stats(n["name"]), nodes))
                sampled = time.monotonic()
            time.sleep(2)
        end = parallel(lambda n: self.stats(n["name"]), nodes)
        elapsed = time.monotonic() - start
        after = self.counts()
        phase = {"label": label, "count": len(nodes), "elapsedSeconds": elapsed, "nodes": nodes,
                 "initial": initial, "final": end, "samples": samples, "health": health,
                 "storageRequests": {k: v - before.get(k, 0) for k, v in after.items() if v != before.get(k, 0)}}
        phase["summary"] = {"meanWorkingMiB": statistics.mean(s["working"] for s in end) / MIB,
            "maxWorkingMiB": max(s["working"] for sample in samples + [end] for s in sample) / MIB,
            "totalWorkingMiB": sum(s["working"] for s in end) / MIB,
            "cpuMillicoresPerFleet": sum(b["cpuNS"] - a["cpuNS"] for a, b in zip(initial, end)) / elapsed / 1e6 / len(nodes),
            "storageRequestsPerFleetSecond": sum(phase["storageRequests"].values()) / elapsed / len(nodes)}
        self.result["phases"].append(phase)
        self.save()
        self.log(json.dumps(phase["summary"]))
        return phase

    def exercise(self, nodes, cells=32):
        def workload(n):
            successes, errors, latencies = 0, [], []
            for cell in range(cells):
                value = n["prefix"] + ":" + str(cell) + ":" + "x" * (128 * 1024)
                url = n["url"] + "?cell=cell-" + str(cell)
                started = time.monotonic()
                try:
                    status, body = request(url, value.encode(), "PUT")
                    if status != 200:
                        raise RuntimeError(f"PUT {status}: {body[:250]}")
                    status, body = request(url)
                    if status != 200 or json.loads(body).get("value") != value:
                        raise RuntimeError(f"GET {status}: incorrect value")
                    successes += 1
                except Exception as e:
                    errors.append(str(e))
                latencies.append(time.monotonic() - started)
                if len(errors) >= 3:
                    break
            return {"name": n["name"], "cells": cells, "successfulWriteReadPairs": successes, "errors": errors,
                    "pairSeconds": latencies, "state": self.api("/containers/" + n["name"] + "/json")["State"]}
        self.log(f"Exercising {len(nodes)} fleets, {cells} distinct cells each")
        results = parallel(workload, nodes)
        self.result.setdefault("workloads", []).append(results)
        self.save()
        return results

    def stop(self, nodes):
        def remove(n):
            cmd("docker", "stop", "-t", "35", n["name"])
            state = self.api("/containers/" + n["name"] + "/json")["State"]
            logs = cmd("docker", "logs", n["name"])
            cmd("docker", "rm", n["name"])
            self.containers.remove(n["name"])
            return {"name": n["name"], "state": state, "logTail": logs[-5000:]}
        result = parallel(remove, nodes, 16)
        self.result.setdefault("stops", []).extend(result)
        self.save()

    def recovery(self, node):
        value = "recovery-" + uuid.uuid4().hex
        status, _ = request(node["url"] + "?cell=recovery", value.encode(), "PUT")
        if status != 200:
            raise RuntimeError("Recovery seed failed")
        self.stop([node])
        start = time.monotonic()
        replacement = self.start(0, "small", node["limitMiB"], node["prefix"])
        status, body = request(replacement["url"] + "?cell=recovery")
        self.result.setdefault("recoveries", []).append({"old": node, "new": replacement, "secondsToRead": time.monotonic() - start,
            "status": status, "valueRestored": status == 200 and json.loads(body).get("value") == value})
        self.save()
        self.stop([replacement])

    def cleanup(self):
        for name in list(self.containers):
            try:
                logs = cmd("docker", "logs", name)
                self.result.setdefault("cleanupLogs", {})[name] = logs[-3000:]
                cmd("docker", "rm", "-f", name)
            except subprocess.CalledProcessError:
                pass
        if self.network:
            try:
                cmd("docker", "network", "rm", self.name)
            except subprocess.CalledProcessError:
                pass
        self.temp.cleanup()
        self.save()

    def execute(self):
        try:
            self.setup()
            if self.args.mode == "smoke":
                nodes = [self.start(0, "small", 128)]
                self.measure(nodes, "smoke", 5)
                self.exercise(nodes, 2)
                self.recovery(nodes[0])
            elif self.args.mode == "quiet":
                nodes = [self.start(i, "quiet", 128) for i in range(2)]
                self.measure(nodes, "quiet-2-idle", max(185, self.args.seconds))
                # Check the same logical key after *both* fleets have written,
                # so immediately reading our own write cannot hide an alias.
                for node in nodes:
                    status, _ = request(node["url"] + "?cell=isolation", node["prefix"].encode(), "PUT")
                    if status != 200:
                        raise RuntimeError("Isolation seed failed")
                checks = []
                for node in nodes:
                    status, body = request(node["url"] + "?cell=isolation")
                    checks.append({"name": node["name"], "correct": status == 200 and json.loads(body).get("value") == node["prefix"]})
                self.result["isolationAfterBothWrites"] = checks
                self.save()
                self.stop(nodes)
            elif self.args.mode == "limits":
                for limit in self.args.limits:
                    try:
                        nodes = [self.start(i, "small", limit) for i in range(2)]
                        self.measure(nodes, f"small-{limit}-idle")
                        self.exercise(nodes)
                        self.measure(nodes, f"small-{limit}-post-load")
                        self.recovery(nodes.pop())
                        self.stop(nodes)
                    except Exception as e:
                        self.result.setdefault("limitFailures", []).append({"limitMiB": limit, "error": str(e)})
                        self.save()
                        self.log(f"Limit {limit}: {e}")
                        # Failed fleets must not contaminate subsequent measurements.
                        failed = [n for n in self.containers if f"-small-{limit}-" in n]
                        for name in failed:
                            self.result.setdefault("failedLogs", {})[name] = cmd("docker", "logs", name)[-5000:]
                            cmd("docker", "rm", "-f", name)
                            self.containers.remove(name)
            else:
                if not self.args.skip_baseline:
                    nodes = parallel(lambda i: self.start(i, "baseline", 1024), range(10), 16)
                    self.measure(nodes, "baseline-10-idle")
                    self.exercise(nodes, 1)
                    self.measure(nodes, "baseline-10-one-cell")
                    self.stop(nodes)
                nodes = []
                for target in self.args.counts:
                    # Conservative cap: proposed runtime working set plus all existing
                    # container working sets must fit in 75% of VM memory.
                    running = self.api("/containers/json")
                    current = sum(s["working"] for s in parallel(lambda c: self.stats(c["Id"]), running))
                    estimate = max(48 * MIB, max((self.stats(n["name"])["working"] for n in nodes), default=0))
                    if current + (target - len(nodes)) * estimate > .75 * self.result["host"]["MemTotal"]:
                        self.result["scaleStop"] = {"target": target, "reason": "75% memory headroom guard", "currentBytes": current, "perNewFleetBytes": estimate}
                        break
                    nodes.extend(parallel(lambda i: self.start(i, "small", 128), range(len(nodes), target), 16))
                    self.measure(nodes, f"small-{target}-idle")
                if nodes:
                    self.exercise(nodes, 1)
                    self.measure(nodes, f"small-{len(nodes)}-one-cell")
                    self.result["writableLayerBytes"] = parallel(lambda n: {"name": n["name"], "bytes": self.api("/containers/" + n["name"] + "/json?size=1").get("SizeRw")}, nodes)
                    self.recovery(nodes.pop())
                    self.stop(nodes)
            self.result["completed"] = True
        except Exception as e:
            self.result["error"] = str(e)
            raise
        finally:
            self.cleanup()


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--mode", choices=["smoke", "limits", "density", "quiet"], default="smoke")
    p.add_argument("--limits", nargs="+", type=int, default=[128, 256])
    p.add_argument("--skip-baseline", action="store_true", help="Run only the selected small-profile density stages")
    p.add_argument("--seconds", type=int, default=65)
    p.add_argument("--counts", nargs="+", type=int, default=[10, 30, 50, 100])
    p.add_argument("--output", required=True)
    Experiment(p.parse_args()).execute()
