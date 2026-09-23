# Builds static arm64 llama.cpp binaries that run on Android without the NDK
# (static musl: no dependency on bionic or /system libs). See ADR-005.
FROM alpine:3.22 AS build
RUN apk add --no-cache build-base cmake git linux-headers
ARG LLAMA_TAG=b11136
# armv8.2-a+dotprod covers Snapdragon 855 and newer. Snapdragon 845 (Kryo 385)
# lacks dotprod: build it with ARM_ARCH=armv8.2-a+fp16.
ARG ARM_ARCH=armv8.2-a+dotprod+fp16
# Bounded parallelism: unbounded -j OOM-killed an 8 GB build VM.
ARG JOBS=4
RUN git clone --depth 1 --branch ${LLAMA_TAG} https://github.com/ggml-org/llama.cpp /src
WORKDIR /src
RUN cmake -B build -DCMAKE_BUILD_TYPE=Release \
      -DBUILD_SHARED_LIBS=OFF -DCMAKE_EXE_LINKER_FLAGS=-static \
      -DGGML_NATIVE=OFF -DGGML_CPU_ARM_ARCH=${ARM_ARCH} \
      -DGGML_OPENMP=OFF -DLLAMA_CURL=OFF -DLLAMA_BUILD_TESTS=OFF \
 && cmake --build build -j ${JOBS} --target llama-bench llama-server \
 && mkdir /out && cp build/bin/llama-bench build/bin/llama-server /out/ && strip /out/*

FROM scratch
COPY --from=build /out/ /
