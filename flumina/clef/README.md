# Clef on Fireworks (Flumina app)

Serves Cloudflare's Clef decision model (27B, or the 9B "flash") as an HTTP endpoint on
Fireworks, with the same request and response bodies as the Jev System One API that
`internal/labeler` already sends, plus pictures and many posts per request. It is for
relabeling posts with a model that can see their images.

**Status (2026-10-02).** Written and checked locally with the 9B weights on an RTX 4090: the app's
own offline check passes (single and batched requests agree, a bad request in a batch is isolated,
pictures work) and `flumina validate` passes. The 27B has not run anywhere yet: it does not fit the
local GPU, so it shares this code path but is untested.

**Deploy attempt (2026-10-02 07:04 UTC): refused by Fireworks.** `flumina deploy clef-9b` validated
the app, then failed at the upload step with `403: flumina model upload is restricted, please contact
Fireworks AI for support` (account `bskyweb`). Nothing was uploaded or billed, and the empty model
record the attempt created was deleted. Flumina uploads need to be enabled by Fireworks for the
account; their blog points to a request form for "guided, alpha usage". Until then this app cannot
be deployed there. The code is plain Python around the Clef loader, so the same logic could be put
behind an ordinary HTTP server on any GPU host.

`data/clef/` currently holds the 9B (`clef-flash`) weights; the 27B weights are kept outside the
app folder at `/data/clef/clef-27b-weights` (a 51 GB copy) so a deploy cannot upload them by accident.

## What Flumina is today

Flumina lets you deploy your own Python as a Fireworks endpoint. Its docs call it an "early
preview", and the tooling needs care:

- The `flumina` command exists only in the old `fireworks-ai` 0.x releases, installed with
  `fireworks-ai[flumina]==0.19.20` (October 2025). The 1.x rewrite has no Flumina.
- That release also needs `uvicorn`, which it does not declare.
- Whether Fireworks still accepts new Flumina deployments, how big an app can be, and how long it
  may take to start are not in the docs. The first deploy is the test.

```
uv venv --python 3.12 flumina-venv
uv pip install --python flumina-venv/bin/python "fireworks-ai[flumina]==0.19.20" uvicorn \
    torch torchvision "transformers==5.17.0" accelerate pillow safetensors huggingface_hub
```

## Files

| File | |
|---|---|
| `flumina.py` | the app: routes `/systemone`, `/systemone_batch`, `/info` |
| `joint_schema_model.py` | Cloudflare's loader for the Clef release (Apache-2.0); the copy beside the weights is used when present |
| `requirements.txt` | installed by Fireworks before the app starts |
| `fireworks.json`, `.fluminaignore` | made by `flumina init app`; leave alone |
| `call.py` | calls a deployed app (standard library only) |
| `data/clef/` | the weights (not in git) |

## Weights

Fireworks recommends packaging weights inside the app rather than downloading them at start-up.
Put the whole release in `data/clef/` as real files:

```
python -c "from huggingface_hub import snapshot_download; \
  snapshot_download('Cloudflare/clef', local_dir='flumina/clef/data/clef')"      # 27B, about 54 GB
echo clef > flumina/clef/data/clef/variant.txt                                   # name reported by /info
```

For the 9B use `Cloudflare/clef-flash` (about 19 GB). As a fallback, `CLEF_HF_REPO=Cloudflare/clef`
makes the app download the release itself when `data/clef/` is missing.

## Check it locally (9B only; the 27B needs 80 GB)

```
cd flumina/clef
CLEF_MODEL_DIR=/data/clef/clef-flash /data/clef/flumina-venv/bin/python flumina.py
CLEF_MODEL_DIR=/data/clef/clef-flash /data/clef/flumina-venv/bin/flumina validate
```

`validate` starts the app, so it needs the weights too. The first prints each answer and ends with
`ok:` and the GPU memory in use (17.8 GiB for the 9B).

## Deploy (costs money: only with the owner's go-ahead)

```
flumina set-api-key <FIREWORKS_API_KEY>
cd flumina/clef
flumina deploy clef-27b --accelerator-type H100 --min-replica-count 1
```

- An H100 (80 GB) is $8.00 an hour while a replica runs. `--min-replica-count` defaults to 0,
  which scales to zero and then reloads 54 GB on the next request; use 1 for a labeling run and
  delete the deployment afterwards, or it keeps billing.
- `flumina deploy` prints a curl command with the endpoint and the `deployment=` id.
- Remove it with `flumina list deployments`, then `flumina delete ...`: deployments first, then
  the model.

## Calling it

```
POST https://api.fireworks.ai/inference/v1/workflows/accounts/<account>/models/<app>/systemone?deployment=accounts/<account>/deployments/<id>
Authorization: Bearer <FIREWORKS_API_KEY>
```

```
FIREWORKS_API_KEY=... python call.py --account A --app clef-27b --deployment accounts/A/deployments/ID info
FIREWORKS_API_KEY=... python call.py --account A --app clef-27b --deployment accounts/A/deployments/ID ask --image photo.jpg
```

Request body: `{"model", "state", "questions", "images": [base64...], "max_image_side": 1000}`; at most 4
images; pictures are shrunk to `max_image_side` pixels on the long side before the model sees them.
`/systemone_batch` takes `{"requests": [...]}` and returns `{"results": [{"response"} | {"error"}]}`
in order.

## Expected speed (estimates, not measured on Fireworks)

One H100, text-only posts (about 3,200 tokens each, two requests per post): 9B about 4-8 posts a
second; 27B about 1.3-3. With a picture shrunk to 1000 px (about 800 tokens) the cost rises
modestly; at full size (about 6,000 tokens) a post takes several times as long. Measured on the
4090 for the 9B: 1.85 posts a second, text only.

## Not done yet

- Deploying, and learning whether Fireworks still takes Flumina apps of this size.
- Running the 27B at all.
- A client for the Go labeler: it must POST to the URL above (with the `deployment=` query) and
  attach base64 pictures from the image archive (`cmd/images`, `/data/images`).
