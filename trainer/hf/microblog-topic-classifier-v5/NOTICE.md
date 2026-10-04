# Notice

This repository is MIT licensed (`LICENSE`). It includes or uses the following, under their own licences.

- **Ettin-150M** (`jhu-clsp/ettin-encoder-150m`), MIT, © Johns Hopkins University CLSP: https://huggingface.co/jhu-clsp/ettin-encoder-150m
  The `text.*` tensors in `model.safetensors` were fine-tuned from it, and `text_encoder/` and `tokenizer/` are copies of its config and tokenizer.
- **SigLIP 2 so400m patch16 512** (`google/siglip2-so400m-patch16-512`), Apache-2.0: https://huggingface.co/google/siglip2-so400m-patch16-512
  Not included. `topic_classifier.py` downloads it and uses it frozen to embed pictures.

No posts or training labels are included.
