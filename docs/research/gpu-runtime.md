# Which local model runtime runs on the RX 7800 XT (gfx1101) behind a CLI or unix socket

Research for issue #16 (child of map #14). Researched 2026-11-25. Planning only: nothing was installed or run on the GPU; the machine was inspected read-only.

## Answer

**llama.cpp is the runtime, used through its Vulkan backend from Arch's official repos.** It is the only candidate that offers *both* transports ADR-0002 allows:

- **(a) per-batch CLI:** `llama-embedding` (embeddings) and `llama-completion` / `llama-cli` (generation), shipped as binaries in Arch's `extra/llama-cpp`.
- **(b) unix socket:** `llama-server --host /path/name.sock` binds an AF_UNIX socket (confirmed in the server source, not just the docs).

**The loopback-TCP fallback (c) is not needed.** Ollama, the obvious TCP route, cannot listen on a unix socket at all (its feature request has been open since 2023), so it stays ruled out.

One catch changes how (b) would be built. `ops/nonetwork_test.go` forbids importing `net/http` anywhere in `cmd/` or `internal/`, *even when the transport is a unix socket*. llama-server speaks HTTP over that socket, so a socket adapter would need a small hand-written HTTP/1.1 client over `net.Dial("unix", …)`, or an explicit, reviewed exception in the test. The per-batch CLI (a) needs only `os/exec` and leaves the test alone. **Recommendation: start with (a). Move to (b) only if measured model-load time per batch turns out to matter.**

Confidence: high on the transports, packaging and GPU support, which come from primary sources. Medium on performance, because nothing ran on this card; the embedding throughput figures below are estimates. A smoke test (`llama-bench` plus one `llama-embedding` batch) should confirm them before the Search PRD fixes numbers.

## Ranked shortlist

| Rank | Runtime | GPU path on gfx1101 | Per-batch CLI | Unix socket | Notes |
|---|---|---|---|---|---|
| 1 | **llama.cpp, Vulkan backend** (`extra/llama-cpp` + `extra/ggml-vulkan`) | Vulkan via Mesa RADV, **already installed and working** | yes: `llama-embedding`, `llama-completion` | yes: `llama-server --host *.sock` | Adds about 60 MB of packages. No ROCm math libraries needed. Serves embeddings and generation. |
| 2 | **llama.cpp, HIP/ROCm backend** (`extra/llama-cpp` + `extra/ggml-hip`) | ROCm. Arch's rocBLAS ships gfx1101 kernels | same binaries | same | Pulls in `rocblas` + `hipblas` (about 2.2 GB) that are not installed now. Worth it only if the smoke test shows HIP clearly faster than Vulkan. |
| 3 | PyTorch-ROCm + sentence-transformers, behind our own script | ROCm (`extra/python-pytorch-rocm` needs the full `rocm-hip-sdk`, MIOpen, etc.) | only a script we would write | only a server we would write | Large install, Python in the loop, and AMD's own Radeon page notes a PyTorch row "Not supported for Radeon 7000 series". No reason to prefer it. |
| 4 | ONNX Runtime ROCm (`extra/onnxruntime-rocm`) | ROCm execution provider | only a program we would write | no | A library, not a runtime with a CLI. We would have to build the tokenizer, pooling and CLI ourselves. |
| – | Ollama (`extra/ollama-rocm` / `ollama-vulkan`) | ROCm (gfx1101 in its supported list) or Vulkan | `ollama run` is only a client of the HTTP server | **no**: TCP only ([ollama#739](https://github.com/ollama/ollama/issues/739), open) | Ruled out by ADR-0002. |
| – | Lemonade server, RamaLama | both wrap llama.cpp | – | not shown | Add a layer over llama.cpp without adding a transport we need. Not pursued. |

## Facts behind the answer

### This machine (read-only inspection)

- GPU: `rocminfo` reports agent `gfx1101`, "AMD Radeon RX 7800 XT", 60 compute units, plus generic ISA `gfx11-generic`. VRAM is 17,163,091,968 bytes (16 GiB). The Raphael iGPU (`gfx1036`) is also visible, so pin the discrete GPU (see caveats).
- ROCm user space present: `rocm-core`, `hip-runtime-amd`, `rocm-hip-runtime`, `rocm-opencl-runtime`, `rocminfo`, `rocm-smi-lib`, all at Arch version 10.1. The KFD driver is loaded and the user is in the `render` and `video` groups. **Not installed:** `rocblas`, `hipblas`, `miopen`, so no ROCm math library is available yet.
- Vulkan: `vulkan-radeon` (Mesa 26.3.1, RADV). `vulkaninfo` lists "AMD Radeon RX 7800 XT (RADV NAVI32)" as a discrete GPU, Vulkan 1.4.
- No inference runtime is installed: no `ollama`, `llama-server`, `llama-embedding` or `llama-cli` on PATH.
- No Backstory store exists on this machine (Backstory is not installed here), so there was nothing to count for corpus size.
- System RAM is 30 GiB. That matters for MoE models whose expert weights can be offloaded to the CPU.

### AMD's position on gfx1101

- AMD's ROCm Linux system requirements list "AMD Radeon RX 7800 XT | RDNA3 | gfx1101 | ✅". The footnote limits official support to Ubuntu 24.04/22.04 and RHEL 10.1/9.7, so **Arch is a distro build, not AMD-supported**. [ROCm system requirements](https://rocm.docs.amd.com/projects/install-on-linux/en/latest/reference/system-requirements.html)
- AMD's ROCm-on-Radeon matrix lists the RX 7800 XT as supported (page covers up to ROCm 7.2.1). [Radeon native Linux compatibility](https://rocm.docs.amd.com/projects/radeon-ryzen/en/latest/docs/compatibility/compatibilityrad/native_linux/native_linux_compatibility.html)
- Arch's `rocblas` package contains gfx1101 kernel files (from its file list). [archlinux.org rocblas files](https://archlinux.org/packages/extra/x86_64/rocblas/files/)
- Because gfx1101 is a native target, no `HSA_OVERRIDE_GFX_VERSION` workaround is needed.

### llama.cpp: packaging, binaries, transports

- Arch now packages llama.cpp officially: `extra/llama-cpp 1.0.0`, built with `LLAMA_USE_SYSTEM_GGML=ON`. It replaces the old `llama-cpp-rocm` and `llama-cpp-vulkan` packages. [PKGBUILD](https://gitlab.archlinux.org/archlinux/packaging/packages/llama-cpp/-/blob/main/PKGBUILD)
- GPU backends are separate, dynamically loaded packages built from one `ggml` source: `ggml-vulkan` (depends only on `vulkan-icd-loader`, about 41 MB) and `ggml-hip` (depends on `hipblas`, `rocblas`, `rocm-hip-runtime`, about 1.16 GB). The HIP build uses `-DAMDGPU_TARGETS=$(rocm-supported-gfx)`. [ggml PKGBUILD](https://gitlab.archlinux.org/archlinux/packaging/packages/ggml/-/blob/main/PKGBUILD)
- The `llama-cpp` package installs `/usr/bin/llama-embedding`, `llama-server`, `llama-cli`, `llama-completion`, `llama-bench` and others, plus system and user `llama-server.service` units. The user unit has `RuntimeDirectory=llama/server`, a natural home for a socket. [file list](https://archlinux.org/packages/extra/x86_64/llama-cpp/files/), [user unit](https://gitlab.archlinux.org/archlinux/packaging/packages/llama-cpp/-/blob/main/llama-server-user.service)
- **Unix socket:** `--host`: "IP addresses to listen on, comma-separated, or UNIX socket paths ending in .sock". In the source, `tools/server/server-http.cpp` checks `string_ends_with(host, ".sock")` and calls `set_address_family(AF_UNIX)`; the listening address is then reported as `unix://…`. The protocol over the socket is still HTTP (the OpenAI-style `/v1/embeddings` and `/v1/chat/completions`, plus a native `/embeddings`). [server README](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md), [server-http.cpp](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/server-http.cpp)
- **Per-batch embeddings CLI:** `llama-embedding` takes many inputs in one run, split by `--embd-separator` (or read from a file with `-f`). `--embd-output-format json` prints OpenAI-style JSON (`array` and `raw` also exist), with `--pooling` and `--embd-normalize` (default: Euclidean). [embedding README](https://github.com/ggml-org/llama.cpp/blob/master/examples/embedding/README.md)
- **One runtime, both uses:**
  - Per-batch: the same package's `llama-embedding` and `llama-completion` binaries.
  - Server: one `llama-server` per model. `--embedding` restricts an instance to embeddings ("use only with dedicated embedding models"), so a single-model server does *not* serve both.
  - Router mode (`llama-server` with no model, plus `--models-dir` / `--models-preset`, `--models-max`, `--models-memory-margin`, `--sleep-idle-seconds`) does load several models on demand. **Caveat:** the router spawns child `llama-server` instances and says it controls their "host, port", which suggests the children listen on TCP behind the router. That is the runtime's own traffic, not Backstory's, but it is not a pure-unix-socket setup. Prefer two single-model servers on two `.sock` paths, or the CLI.
- Ollama's own docs list gfx1101 among supported ROCm targets and offer a Vulkan backend. Its server only listens on TCP: [ollama#739](https://github.com/ollama/ollama/issues/739) "Add ability to listen on unix socket" is still open, last active 2026-04, and a PR attempt (#8072) was not merged. [Ollama GPU docs](https://github.com/ollama/ollama/blob/main/docs/gpu.mdx)

### Performance evidence

- In llama.cpp's Vulkan scoreboard, an RX 7800 XT runs Llama-2-7B Q4_0 at **pp512 ≈ 2017 t/s and tg128 ≈ 118 t/s** (with flash attention ≈ 2197 / 125). The RX 7900 XTX does about 3727 / 183 on the same test. [llama.cpp discussion #10879](https://github.com/ggml-org/llama.cpp/discussions/10879)
- **Embedding throughput (estimate, not measured):** embedding is all prompt processing. A 0.3–0.6B embedding model costs roughly 10–25× less compute per token than a 7B model, so tens of thousands of tokens per second is a plausible order of magnitude. Records are short, so the total is likely to be dominated by model load time (a few hundred MB from disk) on small batches. **The smoke test must measure both** load time and tokens/s, with `llama-bench` and a timed `llama-embedding` run on Vulkan, and on HIP if that backend is installed.
- No primary source compares Vulkan and HIP speed on gfx1101. Pick between them by measurement.

## Embedding models (all have official or ggml-org GGUFs usable by llama.cpp)

| Model | Params | Dims (MRL) | Context | Licence | Quality notes (from model cards) | GGUF |
|---|---|---|---|---|---|---|
| **EmbeddingGemma 2** (2026-09) | 270M text backbone (740M with optional vision/audio) | 768 (512/256/128) | 8,192 | Apache-2.0 | MTEB eng v2 68.46; multilingual v2 61.36; **MTEB code v1 78.68** (≈14% better on code than v1). Near-lossless down to 256d. | [ggml-org/embeddinggemma-2-GGUF](https://huggingface.co/ggml-org/embeddinggemma-2-GGUF) (Q8_0 ≈ 310 MB, BF16 ≈ 558 MB) |
| **Qwen3-Embedding-0.6B** (2025-06) | 0.6B | 1024 (32–1024) | 32k | Apache-2.0 | MTEB eng v2 70.70; multilingual 64.33. Instruction-aware: queries take a prompt. Card says it covers programming languages. | [Qwen/Qwen3-Embedding-0.6B-GGUF](https://huggingface.co/Qwen/Qwen3-Embedding-0.6B-GGUF) (q8_0, f16). Needs `--pooling last` |
| Qwen3-Embedding-4B | 4B | 2560 (32–2560) | 32k | Apache-2.0 | Stronger but about 7× the compute of 0.6B. Overkill for short Records. | official GGUF |
| nomic-embed-text-v1.5 | 137M | 768 (Matryoshka) | 8,192 | Apache-2.0 | Older (2024) but small, fast and proven in llama.cpp. | community GGUFs |
| bge-m3 | 568M | 1024 | 8,192 | MIT | Multilingual dense, sparse and multi-vector. llama.cpp uses only the dense output. | community GGUFs |
| granite-embedding-small-english-r2 | 47M | 384 | 8,192 | Apache-2.0 | Tiny and English-only; card reports CoIR (code) scores. | – |

Not recommended: `google/embeddinggemma-300m` (v1) is under the Gemma licence and gated; v2 replaced it under Apache-2.0. `jina-embeddings-v5-omni-small` is CC-BY-NC-4.0. `nomic-embed-code` is a 7B model, too heavy for an always-available index.

**Pick:** EmbeddingGemma 2 at 768d (or 256d to save storage) as the default: smallest, permissive licence, best code score here. Qwen3-Embedding-0.6B is the alternative if longer inputs or higher English retrieval scores matter. Either way, store the model id and dimension with each vector. Switching models means re-embedding everything, and Delete and Purge must remove vectors too.

Sources: model cards on Hugging Face: [EmbeddingGemma 2](https://huggingface.co/google/embeddinggemma-2), [Qwen3-Embedding-0.6B](https://huggingface.co/Qwen/Qwen3-Embedding-0.6B), [Qwen3-Embedding-4B](https://huggingface.co/Qwen/Qwen3-Embedding-4B), [nomic-embed-text-v1.5](https://huggingface.co/nomic-ai/nomic-embed-text-v1.5), [bge-m3](https://huggingface.co/BAAI/bge-m3), [granite-embedding-small-english-r2](https://huggingface.co/ibm-granite/granite-embedding-small-english-r2), [jina-embeddings-v5-omni-small](https://huggingface.co/jinaai/jina-embeddings-v5-omni-small).

## Generation models for dreams (16 GB VRAM)

The roadmap already says model calls go through a Harness run headless (`claude -p`, `codex exec`). Local generation is therefore an *option* for Dreaming, not a given. If it is wanted, the same llama.cpp install serves it.

| Model | Type | GGUF size (ggml-org) | Fits 16 GB? | Context (card) | Licence |
|---|---|---|---|---|---|
| **gpt-oss-20b** | MoE, MXFP4 | 12.11 GB | yes. The card says it runs "within 16GB of memory" | 128k class | Apache-2.0 |
| **Gemma 4 12B-it** | dense | Q4_0 7.22 GB / Q8_0 12.67 GB | yes, with room for a long KV cache at Q4_0 | 256K | Apache-2.0 |
| Gemma 4 26B-A4B-it | MoE, 4B active | Q4_0 14.62 GB | tight: little room left for context, or offload experts to the CPU | 256K | Apache-2.0 |
| Qwen3.6-35B-A3B | MoE, 3B active | Q4_K_M 20.54 GB | no, unless experts are partly offloaded to CPU RAM (30 GiB available) | – | Apache-2.0 |
| Qwen3.8-27B | dense | Q4_K_M 19.40 GB | no | – | Apache-2.0 |

**Pick if local:** Gemma 4 12B-it Q4_0 (most headroom for context) or gpt-oss-20b (stronger reasoning, about 4 GB left for KV cache). Generation and embeddings cannot both stay loaded at full context. EmbeddingGemma 2 is about 0.3 GB, so it can sit beside a 12 GB generator, but not beside a 14.6 GB one with a large context. Schedule dream runs apart from embedding batches, or use the per-batch CLI so nothing stays resident.

Sources: [ggml-org GGUF repos](https://huggingface.co/ggml-org) (file sizes from the HF API), [gpt-oss-20b card](https://huggingface.co/openai/gpt-oss-20b), [Gemma 4 12B-it card](https://huggingface.co/google/gemma-4-12B-it), [Qwen3.6-35B-A3B card](https://huggingface.co/Qwen/Qwen3.6-35B-A3B).

## Caveats for the decisions that follow

1. **net/http is banned even over unix.** A socket adapter means a hand-rolled HTTP/1.1 client (request line, Content-Length, read and parse the JSON body) or an explicit test exception. The CLI adapter avoids this. This is the main input to the Search-seam / embeddings PRD.
2. **Two GPUs are visible.** Vulkan sees the iGPU too. Pin the discrete card (`GGML_VK_VISIBLE_DEVICES` for Vulkan, `HIP_VISIBLE_DEVICES` / `ROCR_VISIBLE_DEVICES` for HIP) or check `--list-devices` in the smoke test.
3. **Arch ROCm is not AMD-supported** (AMD lists only Ubuntu and RHEL). Vulkan through Mesa avoids that dependency entirely, which is another reason to rank Vulkan first.
4. **Rolling release.** `llama-cpp`, `ggml-*` and Mesa update together on Arch. Pin the model file and record its id, and treat a backend change as something the smoke test re-checks.
5. **Installation is the Human's job.** The minimal set is `pacman -S llama-cpp ggml-vulkan`, plus a GGUF download from Hugging Face by the Human, since Backstory opens no network connection. Backstory should *detect* the binary and model (as `install --check` does for other slots) and never fetch them.
6. **Not measured here:** Vulkan vs HIP speed on gfx1101, model load time, embedding tokens/s, and whether EmbeddingGemma 2 GGUF output matches the reference implementation. All belong to the GPU smoke-test task.
