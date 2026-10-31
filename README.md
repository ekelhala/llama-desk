# llama-desk

A small, unauthenticated web control panel for native `llama-server` processes.
One Go binary, with its HTML, CSS, and JavaScript embedded. No database, npm,
Docker, or inference runtime included.

## Run

Requires Linux and Go 1.25+ to build. The inference host only needs the resulting
binary and your own working `llama-server` build.

```sh
go build -o bin/llama-desk ./cmd/llama-desk
./bin/llama-desk \
  --models /srv/models/gguf \
  --llama-server /home/emil/llama.cpp/build/bin/llama-server \
  --data /home/emil/.local/share/llama-desk \
  --listen 127.0.0.1:8090 \
  --downloads 3
```

Open `http://127.0.0.1:8090`. Defaults use `./models`, `./data`, and `llama-server`
from PATH. The model directory must already be mounted when using a separate disk.
Run as the user who owns the models, without sudo. Do not run two llama-desk
instances against the same data directory.

This application intentionally has **no authentication**. Put it behind your
Pangolin access policy. Keep its listener on loopback when Newt runs on the same
host. It grants the ability to delete models and execute your configured runtime
with custom arguments and environment variables.

## First working slice

- **Downloads:** paste exact Hugging Face `/resolve/` GGUF URLs, one per line.
  Bounded parallel transfers, byte progress, speed, cancel and resume. Downloads
  remain `.part` files until size/header checks pass. Resume uses ETag and
  If-Range; changed files restart from byte zero. Existing files are never replaced.
- **Models:** scan the directory recursively; inspect file path, size, modification
  time and saved launch settings; run or explicitly delete a file. Existing GGUFs
  from the home-directory downloader appear automatically. Symlinks are excluded.
- **Servers:** launch your native binary with context, GPU layers, port, arguments,
  and environment settings. See PID, process state, bounded live logs, health,
  and the exact argument vector/environment overrides. Stop gracefully, then kill
  after five seconds if necessary. Run again opens the saved launch form.

Files are the model inventory. Jobs and per-model settings are saved atomically
in `--data/state.json` with owner-only permissions. Environment overrides are
stored there in plaintext. Interrupted jobs can be resumed after restart; model
servers are stopped with the manager and do not automatically restart.

Extra arguments use **one argument per line**, with each value on its own line.
There is no shell evaluation. For example:

```text
--parallel
1
--jinja
```

Environment overrides use `NAME=value`, one per line, for example
`CUDA_VISIBLE_DEVICES=GPU-9682481c-77d1-da9c-c6bc-0c8374984d36`.
The model, host, and port are owned by the manager. Use arguments rather than
`LLAMA_ARG_*` variables. Managed inference processes bind to `127.0.0.1`.

## Deliberate scaffold boundaries

Downloads currently support public, exact GGUF file links. Add all shards for a
split model and launch the first one. For vision, download the projector and add
`--mmproj` plus its absolute path as separate arguments. There is no Hugging Face
browser, gated-repository credential handling, shard discovery, or upstream
checksum verification yet. GGUF inspection currently shows filesystem metadata,
not parsed architecture/tensor metadata. Downloads are not automatically retried;
failed jobs expose the error and can be resumed manually.

The server panel distinguishes process status from the explicit Health action.
It does not yet collect GPU memory/temperature, slot activity, throughput, or
persist server logs. No automatic routing, eviction, model loading, or LiteLLM
registration is performed. LiteLLM remains a separate gateway.

A user-started server already on port 8080 is not adopted or controlled. Stop it
yourself before launching another on that port, or choose a different port.
Native processes started by llama-desk stay in its own process groups.

## Build and release

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/llama-desk ./cmd/llama-desk
```

GitHub Actions tests on pushes/PRs, then builds Linux amd64 and arm64 archives
with SHA-256 files. Download them from the workflow's artifacts. Pushing a `v*`
tag publishes the archives as a GitHub Release. No deployment happens automatically.

On the inference host, a workflow artifact can be retrieved using `gh`:

```sh
gh run list --repo ekelhala/llama-desk --workflow build.yml
gh run download RUN_ID --repo ekelhala/llama-desk \
  --name llama-desk-linux-amd64 --dir /tmp/llama-desk-release
cd /tmp/llama-desk-release
sha256sum -c llama-desk-linux-amd64.tar.gz.sha256
tar -xzf llama-desk-linux-amd64.tar.gz
install -Dm755 llama-desk ~/.local/bin/llama-desk
```

Review [deploy/llama-desk.service](deploy/llama-desk.service) for an optional
systemd unit. Check the paths and model mount before installing it. This repository
does not change your inference server until you install and start it there.
