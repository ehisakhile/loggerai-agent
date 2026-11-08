# LoggerAI server agent

A small program that shows you what your Linux servers are doing, in [LoggerAI](https://loggerai.uk): CPU, memory, disks, network, the busiest processes and, if you allow it, Docker containers and their logs. It's one file with nothing else to install. It can also watch nginx and MySQL.

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

## Docker containers

This is off unless you turn it on. If Docker is installed, the installer asks. In a script, set `LOGGERAI_DOCKER=true`, and `LOGGERAI_DOCKER_LOGS=true` for logs too. To change your mind later, run the installer again with them set to `false`.

**Know what you're granting.** Docker's socket can't be opened read-only, so anything that can use it can control Docker, which is as good as root on that machine. Turning this on adds the agent's service to the `docker` group. The agent only reads: anything except listing containers and reading their details, stats and logs is refused in its code, and there's a test for that. But that limit is in the agent, not in Docker. To enforce it outside the agent, point `docker_socket` at a read-only socket proxy.

With it on, the agent sends each container's name, host name, image, state, health, restarts, CPU, memory and network. Docker's answers also contain each container's environment variables and command. The agent skips those fields when it reads the answer, so it never keeps or sends them.

With logs on, it also sends what your containers write to stdout and stderr, every 15 seconds:

- Known secrets are masked on your server before anything is sent: passwords, tokens, API keys, database URLs and card numbers. Masking can't catch everything, so check it against your own logs.
- To keep a container's logs out completely, give it the label `loggerai.logs=false`.
- Logs start from when the agent starts. It doesn't send old ones.
- A very busy container sends its latest lines each time, so some can be skipped. Containers share the budget evenly, so a loud one can't silence the others.
- LoggerAI keeps them for 24 hours, and needs ClickHouse to store them.

## nginx and MySQL

Both are off until you set them, and each needs one line in `/etc/loggerai/agent.conf`. Restart the service after changing it.

**nginx** needs its status page, on this machine only:

```nginx
server {
    listen 127.0.0.1:8080;
    location /nginx_status { stub_status; allow 127.0.0.1; deny all; }
}
```

```
nginx_status=http://127.0.0.1:8080/nginx_status
```

It sends connections, requests per second and dropped connections.

**MySQL** needs its own login. This one can read server status and nothing else, so it can't see your tables or your data:

```sql
CREATE USER 'loggerai'@'localhost' IDENTIFIED BY 'a-long-random-password' WITH MAX_USER_CONNECTIONS 2;
GRANT PROCESS, REPLICATION CLIENT ON *.* TO 'loggerai'@'localhost';
```

```
mysql_dsn=loggerai:a-long-random-password@tcp(127.0.0.1:3306)/
```

It sends connections, queries per second, slow queries, failed logins, cache hit rate and replica lag. These are counters only: it never reads query text, table names or data. It also works with MariaDB.

In Docker, publish the port on `127.0.0.1` and use that address.

Your first service turns on alerts for a service that stops answering, and for MySQL running out of connections.

## What it doesn't do

- It never sends command lines, environment variables or file contents.
- It opens no ports. It only makes outbound HTTPS calls to LoggerAI.
- It takes no commands from LoggerAI, so nothing on LoggerAI's side can tell it to do anything.
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
| `docker` | `LOGGERAI_DOCKER` | off |
| `docker_logs` | `LOGGERAI_DOCKER_LOGS` | off |
| `docker_socket` | `LOGGERAI_DOCKER_SOCKET` | `/var/run/docker.sock` |
| `nginx_status` | `LOGGERAI_NGINX_STATUS` | off |
| `mysql_dsn` | `LOGGERAI_MYSQL_DSN` | off |

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
