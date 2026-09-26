package main

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ledgerEntry is one acknowledged application write: the cell (a Durable
// Object name) and the deterministic ID the qualification app stored in it.
type ledgerEntry struct{ cell, id string }

// ledgerReadWindow bounds how long one acknowledged read may keep failing. A
// planned removal hands cells off during celld's SIGTERM drain, so the first
// request for a moving cell can briefly time out; that is an availability blip,
// not data loss. A write that stays unreadable for the whole window fails.
const ledgerReadWindow = 60 * time.Second

func (h *harness) record(fleetName string, entries ...ledgerEntry) {
	if h.ledgers == nil {
		h.ledgers = map[string][]ledgerEntry{}
	}
	h.ledgers[fleetName] = append(h.ledgers[fleetName], entries...)
}

// client returns a probe Pod admitted to the fleet's application port,
// creating it when it does not exist.
func (h *harness) client(fleetName string) string {
	name := "client-" + fleetName
	if fleetName == "alpha" {
		name = "client"
	}
	if _, err := h.tryGet("fleets", "pod", name); err != nil {
		h.probe(name, "fleets", map[string]string{"celld.eric.dev/client-of": fleetName})
	}
	return name
}

// writeLedger synchronously writes one acknowledged ID into each of twelve
// distinct cells, spreading the batch across members.
func (h *harness) writeLedger(fleetName string) {
	probe := h.client(fleetName)
	batch := fmt.Sprintf("ack-%d", time.Now().UnixNano())
	for i := range 12 {
		entry := ledgerEntry{cell: fmt.Sprintf("integration-%d", i), id: fmt.Sprintf("%s-%d", batch, i)}
		assert(stored(h.app(probe, fleetName, "PUT", entry.path()), entry.id), "write %s not acknowledged", entry.path())
		h.record(fleetName, entry)
	}
}

func (e ledgerEntry) path() string { return "/?cell=" + e.cell + "&id=" + e.id }

// readScript checks every "cell id" line on stdin through the fleet's
// ClusterIP, retrying each for a bounded window. It prints MISSING for a write
// that never read back as stored and a final VERIFIED count.
const readScript = `n=0
while read -r cell id; do
  [ -n "$id" ] || continue
  deadline=$(( $(date +%s) + WINDOW ))
  last=""
  until case "$last" in *"\"id\":\"$id\",\"stored\":true"*) true;; *) false;; esac; do
    if [ "$(date +%s)" -ge "$deadline" ]; then echo "MISSING $cell $id $last"; break; fi
    last=$(curl --silent --show-error --max-time 3 "http://FLEET:8080/?cell=$cell&id=$id" 2>&1)
    case "$last" in *"\"id\":\"$id\",\"stored\":true"*) ;; *) sleep 1;; esac
  done
  n=$((n+1))
done
echo "VERIFIED $n"
`

// readLedger requires every acknowledged write for the fleet to be readable.
func (h *harness) readLedger(fleetName string) {
	entries := h.ledgers[fleetName]
	assert(len(entries) > 0, "no acknowledged ledger writes for %s", fleetName)
	var input strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&input, "%s %s\n", e.cell, e.id)
	}
	script := strings.NewReplacer("WINDOW", strconv.Itoa(int(ledgerReadWindow.Seconds())), "FLEET", fleetName).Replace(readScript)
	out := h.run(command{
		args:    h.kubectl("-n", "fleets", "exec", "-i", h.client(fleetName), "--", "/bin/sh", "-c", script),
		timeout: 10*time.Minute + time.Duration(len(entries))*time.Second,
		stdin:   input.String(),
	})
	must(verifiedReads(out, len(entries)))
	fmt.Printf("PASS: %s %d/%d acknowledged writes readable\n", fleetName, len(entries), len(entries))
}

func verifiedReads(out string, want int) error {
	var missing []string
	verified := -1
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "MISSING "):
			missing = append(missing, line)
		case strings.HasPrefix(line, "VERIFIED "):
			verified, _ = strconv.Atoi(strings.TrimPrefix(line, "VERIFIED "))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%d acknowledged write(s) lost or unreadable:\n%s", len(missing), strings.Join(missing, "\n"))
	}
	if verified != want {
		return fmt.Errorf("read check covered %d of %d acknowledged writes: %s", verified, want, out)
	}
	return nil
}

// writerScript acknowledges a stream of writes across twelve cells, printing
// one ACK line only for a response that stored exactly the requested ID.
// Failed or ambiguous writes are not acknowledged and are not checked.
const writerScript = `i=0
while [ ! -e /tmp/stop ]; do
  i=$((i+1)); cell="load-$((i % 12))"; id="RUN-$i"
  out=$(curl --silent --fail --max-time 5 -X PUT "http://FLEET:8080/?cell=$cell&id=$id" 2>/dev/null) || out=""
  case "$out" in *"\"id\":\"$id\",\"stored\":true"*) echo "ACK $cell $id";; esac
  sleep 0.5
done
echo STOPPED
exec sleep 3600
`

// startWriter runs continuous write load against a fleet until stopWriter.
func (h *harness) startWriter(fleetName string) {
	name := "writer-" + fleetName
	run := fmt.Sprintf("w%d", time.Now().UnixNano())
	pod := sleepPod(name, "fleets", map[string]string{"celld.eric.dev/client-of": fleetName})
	// The operator's node is never drained or failed, so writes continue
	// through every fault.
	pod.Spec.NodeName = h.nodes[0]
	pod.Spec.Containers[0].Name = "writer"
	pod.Spec.Containers[0].Command = []string{"/bin/sh", "-c", strings.NewReplacer("RUN", run, "FLEET", fleetName).Replace(writerScript)}
	h.apply(pod)
	h.waitFor("continuous writer running against "+fleetName, 120*time.Second, func() bool {
		return str(h.getIn("fleets", "pod", name), "status", "phase") == "Running"
	})
	h.waitFor("continuous writes acknowledged by "+fleetName, 120*time.Second, func() bool {
		return len(acks(h.k("-n", "fleets", "logs", name))) > 0
	})
}

// stopWriter records the writer's acknowledged writes in the ledger and
// removes it.
func (h *harness) stopWriter(fleetName string) {
	name := "writer-" + fleetName
	// Stop the loop first so the log read below is the complete record.
	h.k("-n", "fleets", "exec", name, "--", "touch", "/tmp/stop")
	var logs string
	h.wait("continuous writer against "+fleetName+" stopped", func() bool {
		logs = h.k("-n", "fleets", "logs", name)
		return strings.Contains(logs, "STOPPED")
	})
	entries := acks(logs)
	h.k("-n", "fleets", "delete", "pod", name, "--wait=true", "--grace-period=1")
	assert(len(entries) > 0, "continuous writer against %s acknowledged nothing", fleetName)
	h.record(fleetName, entries...)
	fmt.Printf("PASS: %s acknowledged %d continuous writes\n", fleetName, len(entries))
}

var ackLine = regexp.MustCompile(`^ACK (load-\d+) (w\d+-\d+)$`)

func acks(logs string) []ledgerEntry {
	var out []ledgerEntry
	for line := range strings.SplitSeq(logs, "\n") {
		if m := ackLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			out = append(out, ledgerEntry{cell: m[1], id: m[2]})
		}
	}
	return out
}
