# vpsagent

`vpsagent` is the headless, open-source sender for VPSmon Cloud. It collects host metrics through [vpsmonlib](https://github.com/vpsmon/vpsmonlib) and sends them over outbound HTTPS.

## Helper scripts

```bash
sudo /opt/vpsagent/update.sh
sudo /opt/vpsagent/remove.sh
```

`update.sh` checks the latest release, verifies its checksum, and restores the old binary if the service fails to start. `remove.sh` asks for confirmation and removes the local service and credential.

## Data sent to Cloud

Normal 15-second uploads include host resource usage, uptime/load, network rates,
process count, and writable disk capacity. Read-only Snap/ISO images are excluded
from disk monitoring. The agent opens no inbound port.

Selecting **Show live processes** in Cloud requests up to five process names and
CPU/memory percentages on subsequent heartbeats. These details go to a separate
endpoint, stay briefly in Cloud memory, and never enter the metrics database or
backups. Closing/stopping the view ends requests; the lease expires after 30
seconds without renewal. CPU comes from `ps` and is the process lifetime average.
Command arguments, environment variables, logs, sockets, and containers are not
sent in this live view.

Saved incident process/container snapshots remain a separate option, disabled
unless `VPSAGENT_INCIDENT_SNAPSHOTS=true`. Normal Cloud resource uploads continue
if a live-process request fails.

## Collection freshness

Cloud uploads require a measurement collected within the last minute. The agent
uploads each advancing collection timestamp once, skips frozen samples, and
resumes when fresh collection returns. Cloud also persists timestamp progress so
new upload IDs cannot make an old measurement look current. Keep the VPS clock
synced with NTP; Cloud tolerates five minutes of clock skew. A collection conflict
(HTTP 409) is retried without permanently stopping the uploader.

Optional Docker, GPU and process collection shares a three-second deadline and a
1 MiB output limit. Slow optional commands cannot indefinitely freeze core
collection. Update existing installations with `sudo /opt/vpsagent/update.sh`.

## Updates from Cloud

Remote updates are **off by default**. For an existing installation, run these
once on the monitored VPS after the v0.0.9 release is available:

```bash
sudo /opt/vpsagent/update.sh
sudo /opt/vpsagent/vpsagent enable-remote-updates
```

Then open **Server details → Server settings → Update agent** in Cloud. Cloud
chooses the latest published signed release; updates happen only when you press
the button. A new install can opt in with `install.sh --enable-remote-updates`.
Disable requests locally with:

```bash
sudo /opt/vpsagent/vpsagent disable-remote-updates
```

A separate outbound HTTPS control check runs every 30 seconds, even if resource
collection stalls or telemetry is paused by billing. It reports release version,
Linux architecture, local opt-in state, and update progress. It does not renew
the metrics heartbeat. No SSH credentials or new inbound ports are needed.

The unprivileged agent writes only a bounded request (ID, version, expiry) into
`/var/lib/vpsagent-control/requests`. A separate root systemd path/service pair
accepts only newer releases from `vpsmon/vpsagent`. It verifies the Sigstore
manifest signature against the GitHub release workflow identity and trusted TUF
roots, then checks the binary against the signed SHA-256 manifest. The helper
cannot accept a command, download URL, or installation path from Cloud. Signing
uses GitHub Actions OIDC; no reusable release signing secret is stored in CI.

The helper atomically replaces `/opt/vpsagent/vpsagent`, restarts its service,
and checks that the same service process stays running. Failed startup restores
`vpsagent.previous`. A root-owned journal recovers interrupted installations at
boot; interrupted downloads leave the current binary in place. Cloud retains up
to 20 update jobs per server and expires unfinished jobs after 15 minutes. Keep
the VPS clock synchronized. Updates require outbound HTTPS access to GitHub and
the Sigstore TUF service. A restart briefly interrupts telemetry.

Diagnostics: `journalctl -u vpsagent-update.service` and
`systemctl status vpsagent-update.path`. An unprivileged agent cannot turn on the
root-owned opt-in marker. Disabling requests allows a verified update already in
progress to finish. Removal stops the updater and removes its managed state.

Tests: `go test -race ./...`. The isolated Linux/systemd integration test is
explicit: build `go test -c ./internal/update`, then run the test binary as root
with `VPSAGENT_SYSTEMD_TEST=1 -test.run TestSystemdUpdateAndRollback`. It uses
unique temporary units, exercises an unprivileged request in a read-only mount
namespace, tests success and failed-start rollback, and cleans up its own units.
