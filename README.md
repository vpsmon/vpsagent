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
