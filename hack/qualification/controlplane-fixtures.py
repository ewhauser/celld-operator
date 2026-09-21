#!/usr/bin/env python3
"""Generate wire fixtures from the runtime's actual snapshot and Control sources.

This compiles only serialization/state code, not celld or its shutdown machinery.
Usage: python3 hack/qualification/controlplane-fixtures.py /path/to/celld
Requires cargo and cached serde_json (runs offline). Never modifies the source tree.
"""

import hashlib
import json
import pathlib
import subprocess
import sys
import tempfile

root = pathlib.Path(sys.argv[1]).resolve()
source = root / "crates/celld/disk_removal.rs"
logic = root / "crates/logic/disk_removal.rs"
text = source.read_text()
start = text.index("pub struct State {")
end = text.index("#[derive", start)
state = text[start:end]
output_path = (
    pathlib.Path(__file__).resolve().parents[2]
    / "internal/runtime/controlplane/testdata/shutdown-v1.json"
)

with tempfile.TemporaryDirectory(prefix="celld-control-contract-") as temp:
    path = pathlib.Path(temp)
    (path / "src").mkdir()
    (path / "Cargo.toml").write_text(
        '[package]\nname="celld-control-contract"\nversion="0.0.0"\nedition="2024"\n'
        '[dependencies]\nserde_json="1"\n'
    )
    harness = "#![allow(dead_code)]\nuse std::sync::{Mutex,atomic::{AtomicBool,Ordering}};\n"
    harness += (
        f"#[path={json.dumps(str(logic))}] mod control;\n"
        "use control::{Control,Phase};\n" + state
    )
    harness += '''fn main() {
    let state = State::new("generation-a".into(), true);
    let idle = state.snapshot();
    state.control.lock().unwrap().request("scale-in-42", "generation-a").unwrap();
    let draining = state.snapshot();
    state.control_only.store(true, Ordering::SeqCst);
    state.control.lock().unwrap().finish(Ok(()));
    let safe = state.snapshot();
    let failed = State::new("generation-a".into(), true);
    failed.control.lock().unwrap().request("scale-in-42", "generation-a").unwrap();
    failed.control_only.store(true, Ordering::SeqCst);
    failed.control.lock().unwrap().finish(Err("deadline exceeded".into()));
    println!("{}", serde_json::json!({
        "idle": idle, "draining": draining, "data_safe": safe,
        "failed": failed.snapshot(),
        "unsupported": State::new("generation-a".into(), false).snapshot()
    }));
}
'''
    (path / "src/main.rs").write_text(harness)
    output = subprocess.check_output(
        ["cargo", "run", "--offline", "--quiet", "--manifest-path", str(path / "Cargo.toml")],
        text=True,
    )
    output_path.write_text(json.dumps(json.loads(output), indent=2) + "\n")
    print("snapshot source SHA256", hashlib.sha256(source.read_bytes()).hexdigest())
    print("logic source SHA256", hashlib.sha256(logic.read_bytes()).hexdigest())
