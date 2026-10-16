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
