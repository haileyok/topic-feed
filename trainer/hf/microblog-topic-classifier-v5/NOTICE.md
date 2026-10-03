# Notice

This repository contains parts derived from other projects.

- **Ettin-150M** (`jhu-clsp/ettin-encoder-150m`), MIT licence: https://huggingface.co/jhu-clsp/ettin-encoder-150m
  The fine-tuned weights in `model.safetensors` (every tensor whose name starts with `text.`) were initialised from it, and `text_encoder/` and
  `tokenizer/` are copies of its architecture config and tokenizer files. Copyright remains with its authors.
- **SigLIP 2 so400m patch16 512** (`google/siglip2-so400m-patch16-512`), Apache-2.0 licence: https://huggingface.co/google/siglip2-so400m-patch16-512
  Not included here. `topic_classifier.py` downloads it from its own repository and uses it frozen, to turn pictures into embeddings.

The labels used for training were produced by two LLM-based teachers on public Bluesky posts. No posts or labels are included in this repository.
