# Setting up topic-feed on the Framework Desktop (AMD Ryzen AI Max+ 395)

Instructions for an agent setting up the new machine. Read all of it before starting. Work
through the steps in order and check each one before moving on. Stop and ask the owner where
it says **ASK**.

## Target hardware

- Framework Desktop, AMD Ryzen AI Max+ 395: 16 cores / 32 threads, Radeon 8060S built-in GPU
  (40 compute units, LLVM target **gfx1151**), 128 GB LPDDR5x **shared by the CPU and GPU**.
- No NVIDIA GPU. PyTorch runs on AMD's ROCm. ROCm builds of PyTorch still report the GPU as
  `torch.cuda` / device type `"cuda"`. The code checks `device.type == "cuda"` in
  `trainer/topic_classifier.py` and `trainer/train_fusion.py`, so the GPU path and bf16 switch
  on with no code changes.

## The current machine (for reference)

- Ubuntu 26.04.1, kernel 7.0.0-34, RTX 4090 (24 GB), 246 GB RAM, 48 threads.
- `/data` is a 7.3 TB NVMe drive with 541 GB used. What is on it:
  | path | size | needed for serving? |
  |---|---|---|
  | `/data/models/v5` (inside `/data/models`, 4.5 GB in total) | 583 MB | **yes**, this is the live model |
  | `/data/clickhouse/data` | unknown (owned by the container's user, check with sudo) | see step 6 |
  | `/data/images` | 51 GB | training only |
  | `/data/clef` | 81 GB | training/labelling only |
  | `/data/exports`, `/data/mm`, `/data/judged-sets`, `/data/relabel`, `/data/reports`, `/data/reference`, `/data/hf` | small | training/analysis |
  | `/data/monitoring` | 400 MB | local monitoring copy |
- Hugging Face cache `~/.cache/huggingface` (16 GB). The classifier service runs with
  `HF_HUB_OFFLINE=1`, so these two must be in the cache **before** it starts:
  `jhu-clsp/ettin-encoder-150m` and `google/siglip2-so400m-patch16-512`.
- Secrets: `~/.config/topic-feed/env` (chmod 600). Keys: `TYPESAFE_API_KEY`,
  `JETSTREAM_API_KEY`, `CLICKHOUSE_PASSWORD`, `FEEDGEN_*`.
- Cloudflare tunnel: system service `cloudflared.service`, token in `/etc/cloudflared/token`,
  binary in `/usr/local/bin/cloudflared`. Bluesky reaches the feed generator through it.
- Docker 29.8.1 runs **rootless** (data under `~/.local/share/docker`).
- `uv`, `go` and `just` come from Nix (`~/.nix-profile/bin`). The classifier unit runs
  `%h/.nix-profile/bin/uv`.
- systemd user units: `topic-feed-classifier.service`, `topic-feed-backup.{service,timer}`.
- Running containers: clickhouse, ingest, modlabels, pipeline, feedgen, viewer, tensorboard
  (`deploy/docker-compose.yml`), and the monitoring stack (`~/bluesky/monitoring`).

## What runs where

Everything runs on one machine. Only the classifier service (`trainer/serve.py`) uses the GPU.
It runs on the host under systemd, not in Docker, and listens on `127.0.0.1:8700`. The pipeline
container (host network) sends it batches of posts. The model is a 150M text encoder (Ettin)
plus the SigLIP 2 so400m picture encoder, and it uses about 7 GB of GPU memory. Live load is
about 8 posts/s. This machine has plenty of memory and speed for it.

---

## Step 1: OS and kernel

1. Install Ubuntu 24.04 LTS or newer (26.04 matches the current machine). **ASK** if the owner
   wants something else.
2. Check the kernel against AMD's current ROCm compatibility list for gfx1151 before going
   further. AMD's PyTorch-on-Ryzen documentation has required specific kernels for this chip
   (an Ubuntu 6.14 OEM kernel, with crashes reported on 6.12.0-1018). Use the newest
   combination AMD lists as supported. Starting points:
   - https://rocm.docs.amd.com/ (unified ROCm documentation, ROCm 7.13 and later)
   - https://rocm.docs.amd.com/projects/radeon-ryzen/en/latest/ (older Radeon/Ryzen pages)
3. Update the BIOS and `linux-firmware`.

## Step 2: GPU memory

The CPU and GPU share the 128 GB. The classifier needs about 7 GB of GPU memory; training uses
more. ClickHouse and the containers need system RAM too (the current machine uses about 28 GB).

- In the BIOS, set the dedicated GPU memory (UMA frame buffer) to something moderate, for
  example 16–32 GB. On Linux ROCm can also use shared system memory (GTT) beyond the dedicated
  amount, so a very large dedicated size isn't needed and takes RAM away from ClickHouse.
- Write down what you chose and what `rocm-smi --showmeminfo vram gtt` reports.

## Step 3: ROCm and access to the GPU

1. Install ROCm following AMD's current instructions for gfx1151 (Ryzen AI Max 300 series).
2. Add the user to the GPU groups: `sudo usermod -aG render,video $USER`, then log out and back
   in.
3. Check:
   ```
   rocminfo | grep -m1 gfx1151        # must print gfx1151
   rocm-smi                           # GPU listed, no errors
   ```

## Step 4: Base tools

- Install Nix and put `uv`, `go` and `just` in `~/.nix-profile/bin`. That way the classifier
  unit's `ExecStart=%h/.nix-profile/bin/uv ...` works unchanged. If you install `uv` somewhere
  else, update `deploy/systemd/topic-feed-classifier.service` to match.
- Install Docker (rootless, like the current machine) with the compose plugin. The compose
  services don't use the GPU, so Docker needs no GPU runtime.
- Clone the repos into `~/bluesky`: `topic-feed` (`git@github.com:haileyok/topic-feed.git`)
  and `monitoring`. Copy anything else from `~/bluesky` only if the owner asks.

## Step 5: Switch PyTorch to the ROCm build (the one code change)

`trainer/pyproject.toml` currently installs PyTorch from NVIDIA's CUDA index:

```toml
dependencies = [ "torch==2.14.0", ... ]

[tool.uv.sources]
torch = [{ index = "pytorch-cu132" }]

[[tool.uv.index]]
name = "pytorch-cu132"
url = "https://download.pytorch.org/whl/cu132"
explicit = true
```

Point `torch` at a ROCm index that has gfx1151 builds for Python 3.12 on Linux. Options, in
order of preference:

1. AMD's release wheels. AMD recommends them over the PyTorch Foundation builds for Ryzen.
   As of 2026-10 this listing has gfx1151 builds of torch 2.14.0 for cp312 Linux:
   https://stable.repo.amd.com/rocm/pytorch/whl-next/amd-torch-device-gfx1151/
   **Check how that index is meant to be installed.** The package there is named
   `amd-torch-device-gfx1151`, not `torch`, so read AMD's install docs for the right package
   names and uv/pip setup. Don't guess.
2. AMD's gfx1151 nightly index: `https://rocm.nightlies.amd.com/v2/gfx1151/`
3. The PyTorch Foundation ROCm index (`https://download.pytorch.org/whl/rocmX.Y`), only if it
   lists gfx1151 support.

Keep `torch==2.14.0` if the chosen index has it. If it doesn't, change the pin to the closest
version it has and note that.

Make the change on a branch (for example `framework-rocm`). The current 4090 machine still uses
the CUDA config, so don't push it to `main` until the owner decides how both machines should be
configured. **ASK** before merging.

Then:
```
cd ~/bluesky/topic-feed/trainer
uv lock
uv sync --frozen
uv run python - <<'PY'
import torch
print(torch.__version__, torch.version.hip, torch.cuda.is_available(), torch.cuda.get_device_name(0))
x = torch.randn(4096, 4096, device="cuda", dtype=torch.bfloat16)
print((x @ x).float().abs().mean().item())
PY
```
`torch.version.hip` must not be `None`, `is_available()` must be `True`, and the matmul must
finish without errors. If AMD's docs recommend `TORCH_ROCM_AOTRITON_ENABLE_EXPERIMENTAL=1` for
faster attention on this chip, try it and keep it only if the step 8 checks pass.

## Step 6: Copy data and secrets

Copy from the current machine (call it `penguin`) with `rsync -aHAX --info=progress2`.

**Required for serving:**
- `~/.config/topic-feed/env` → same path, `chmod 600`.
- `/data/models/v5` → `/data/models/v5`. Copying all of `/data/models` (4.5 GB) is fine and
  keeps older models for rollback.
- The two Hugging Face models from `~/.cache/huggingface/hub`:
  `models--jhu-clsp--ettin-encoder-150m` and `models--google--siglip2-so400m-patch16-512`.
  Copy the whole cache (16 GB) if that's simpler.

**ClickHouse.** **ASK** the owner which option to use:
- *Copy everything*: stop the stack on penguin (`make down`), then rsync
  `/data/clickhouse/data` with sudo, keeping ownership. The container runs as uid/gid
  `100100` under rootless Docker, so check that the ownership matches the new machine's
  rootless Docker uid mapping (`/etc/subuid`) and fix it if not.
- *Start fresh*: copy only the backups. `deploy/backup.sh` backs up the tables that can't be
  rebuilt (`jev_labels`, `jev_requests`, `models`) to `~/backups/topic-feed` (4.5 GB). Posts
  and likes refill from Jetstream, but feeds will only have the last 24 hours of posts once
  that much has been ingested.

**Training data** (only if the owner wants to train on this machine): `/data/images`,
`/data/clef`, `/data/exports`, `/data/mm`, `/data/judged-sets`, `/data/relabel`.

## Step 7: Start the services

```
cd ~/bluesky/topic-feed
make up            # clickhouse, ingest, modlabels, pipeline, viewer, tensorboard
make schema        # idempotent; safe either way
make install-classifier
make classifier-logs   # wait for "serving ... on 127.0.0.1:8700"
make feeds         # feed generator
make install-backup
```

## Step 8: Check the classifier on the GPU

```
curl -s 127.0.0.1:8700/healthz
```
`device` must be `cuda:0` (not `cpu`) and `model` must be the v5 model. Then:
- `cd trainer && uv run python test_serve_pipeline.py`. **ASK** if it fails.
- Watch `make classifier-logs` for a few minutes. The service logs timings every 30 seconds
  (`TOPIC_PROFILE_SECONDS`). Compare batch times with the same log on penguin. They will be
  slower than on the 4090. It's fine as long as the pipeline keeps up: posts don't pile up and
  `post_pipeline` keeps filling within seconds of the pipeline delay.
- Compare outputs with penguin: send the same few posts (with and without pictures) to both
  machines' `/classify` and check the topic probabilities nearly match. Small differences from
  bf16 are expected; a different top topic on several posts is not. **ASK** if they differ.

## Step 9: Cutover

Do this only after the owner approves.

1. The Cloudflare tunnel token in `/etc/cloudflared/token` sends Bluesky's feed requests to
   whichever machine runs it. **If the tunnel runs on both machines, requests are split
   between them.** Stop `cloudflared` on penguin, then install and start it on the new machine
   with the same token.
2. Stop the topic-feed stack on penguin (`make down`, then
   `systemctl --user disable --now topic-feed-classifier topic-feed-backup.timer`) so two
   machines aren't ingesting the same stream.
3. Check that a feed loads in the Bluesky app.

## Step 10: Monitoring

`~/bluesky/monitoring` runs Alloy, Prometheus, Loki and node-exporter, and sends to the central
Grafana on unraid (`192.168.88.247`). Update the machine name in its config. It scrapes a
GPU exporter on `:9835` (a host systemd unit, not compose), which reads NVIDIA GPUs and won't
work here. Replace it with an AMD exporter (for example AMD's device-metrics-exporter, or
node-exporter's amdgpu metrics) and update the Grafana GPU panels. **ASK** before changing
anything on unraid.

## Known differences from the 4090

- Training (`train_fusion.py`) works the same way but is several times slower. This GPU has
  much less compute and memory bandwidth than a 4090.
- The labelling work that runs on a rented GPU (`trainer/clef_*.py`) is unaffected.
- `topic-feed-classifier.service` and `deploy/docker-compose.yml` say "GPU" and "Docker here
  has no NVIDIA runtime". Those comments are still accurate. Don't add a GPU runtime to Docker.

## Done when

- [ ] `rocminfo` shows gfx1151 and PyTorch reports a HIP build with the GPU available.
- [ ] `/healthz` reports `cuda:0` and the v5 model.
- [ ] `test_serve_pipeline.py` passes and outputs match penguin's.
- [ ] The pipeline keeps up with live traffic for at least 30 minutes.
- [ ] Feeds load in the Bluesky app over the tunnel from the new machine.
- [ ] The backup timer is installed and the first backup has run (`make backup`).
- [ ] The `pyproject.toml` / `uv.lock` change is on a branch, with what was chosen and why
      written in the commit message.
