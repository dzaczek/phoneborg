#!/bin/bash
# The Big Brain benchmark series of docs/BENCHMARKS.md (2026-10-16): one
# configuration after another on the same pool. Node names are the
# production cluster's aliases; 268ff7f1 is the MI 9.
set -u
cd "$(dirname "$0")"
A=qwen3-30b-a3b-2507-ud-q3_k_xl
D=qwen3-32b-q4_k_m
L=llama-3.3-70b-ud-iq2_xxs
python3 bench.py brain $A pixel,oneplus
python3 bench.py brain $D pixel,oneplus,poco,268ff7f1,mi8
python3 bench.py brain $A pixel,oneplus,poco,268ff7f1,mi8
python3 bench.py brain $A pixel,oneplus,poco,268ff7f1
python3 bench.py brain $A pixel,oneplus,poco
python3 bench.py brain $A pixel,oneplus,mi8
python3 bench.py brain $D pixel,oneplus,poco,268ff7f1
python3 bench.py brain $L pixel,oneplus,poco,268ff7f1,mi8
# Second part, the same evening: architectures other than Qwen and Llama.
python3 bench.py brain gpt-oss-20b-mxfp4 pixel,oneplus
python3 bench.py brain mistral-small-3.2-24b-q4_k_m pixel,oneplus,poco
python3 bench.py brain gemma-3-27b-it-q4_k_m pixel,oneplus,poco
