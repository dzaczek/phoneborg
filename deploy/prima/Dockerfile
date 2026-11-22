# Static arm64 (musl) prima.cpp llama-cli / llama-server for plain adb shell.
FROM alpine:3.22 AS build
RUN apk add --no-cache build-base cmake git linux-headers
ARG ZMQ_TAG=v4.3.5
ARG ARM_ARCH=armv8.2-a+dotprod+fp16
ARG JOBS=4
RUN git clone --depth 1 --branch ${ZMQ_TAG} https://github.com/zeromq/libzmq /zmq \
 && cmake -S /zmq -B /zmq/build -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED=OFF -DBUILD_STATIC=ON \
      -DBUILD_TESTS=OFF -DWITH_PERF_TOOL=OFF -DENABLE_DRAFTS=OFF -DWITH_LIBSODIUM=OFF -DWITH_DOCS=OFF \
      -DCMAKE_INSTALL_PREFIX=/usr/local \
 && cmake --build /zmq/build -j ${JOBS} && cmake --install /zmq/build
RUN git clone https://github.com/OpenCPIL/prima.cpp /src
COPY prima-android.diff /prima-android.diff
WORKDIR /src
RUN git checkout 288df6a2cc2120387e12d6f40798f30cc525c9ea && git apply /prima-android.diff
RUN make -j ${JOBS} llama-cli llama-server GGML_NO_OPENMP=1 GGML_NO_LLAMAFILE=1 \
      ARM_MARCH="-march=${ARM_ARCH}" LDFLAGS=-static \
 && mkdir /out && cp llama-cli llama-server /out/ && strip /out/*
FROM scratch
COPY --from=build /out/ /
