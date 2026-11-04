# LoggerAI server agent

A small program that shows you what your Linux servers are doing, in [LoggerAI](https://loggerai.uk): CPU, memory, disks, network, and the busiest processes. It's one file with nothing else to install.

## Install

```bash
curl -fsSL https://github.com/ehisakhile/loggerai-agent/releases/latest/download/install.sh | sudo sh
```

It asks for your project's secret key, which you'll find under Connect in LoggerAI. The server appears on the Servers page within a minute. Your project's first server also turns on alerts for high CPU, high memory, a full disk, and a server that stops reporting.

It needs Linux with systemd, on Intel or ARM.

To skip the question, for example from a provisioning script:

```bash
curl -fsSL https://github.com/ehisakhile/loggerai-agent/releases/latest/download/install.sh | sudo LOGGERAI_KEY=sk_live_... sh
```

That can leave the key in your shell history, so prefer the first form by hand.

Run the install command again to upgrade. Your settings stay.

## What it sends

Once a minute:

- CPU, memory, swap and load
- Each local disk's mount point, size and how full it is
- Network and disk throughput
- The names of the busiest processes, added together by name, with their CPU and memory
- The machine's name, operating system, kernel and when it started

To see exactly what it would send, without sending anything:

```bash
loggerai-agent print
```

## What it doesn't do

- It never sends command lines, environment variables or file contents.
- It opens no ports. It only makes outbound HTTPS calls to LoggerAI.
- It takes no commands from LoggerAI, so a compromised server can't tell it to do anything.
- It runs as its own unprivileged user, in a locked-down systemd service limited to 128 MB of memory and 10% of one CPU.
- It doesn't follow redirects, so your key can't be sent anywhere else.

Your key is stored in `/etc/loggerai/agent.conf`, readable only by root and the agent.

## If LoggerAI can't be reached

The agent keeps up to an hour of reports and sends them, oldest first, when the connection is back. Logs are in `journalctl -u loggerai-agent`.

## Settings

In `/etc/loggerai/agent.conf`, or as environment variables. Restart the service after changing the file.

| Setting | Environment variable | Default |
|---|---|---|
| `key` | `LOGGERAI_KEY` | none |
| `endpoint` | `LOGGERAI_ENDPOINT` | LoggerAI's cloud |
| `environment` | `LOGGERAI_ENVIRONMENT` | `production` |
| `host` | `LOGGERAI_HOST` | the machine's name |
| `allow_http` | `LOGGERAI_ALLOW_HTTP` | off. The endpoint must use https unless it's this machine |

`loggerai-agent check` sends one report, to test the key and connection.

## Remove it

```bash
curl -fsSL https://github.com/ehisakhile/loggerai-agent/releases/latest/download/install.sh | sudo sh -s uninstall
```

Then remove the server from the Servers page, or its "stopped reporting" alert keeps firing.

## Develop

```bash
go test ./...
```

A tag like `v0.1.0` builds the Linux binaries and publishes them, with `install.sh` and checksums, as a GitHub release.

## Licence

MIT
