# ── Stage 1: build NFS-Ganesha from source ───────────────────────────────────
# Mirrors exactly the SaunaFS CI workflow:
# https://github.com/leil-io/saunafs/blob/dev/.github/workflows/run-unit-and-ganesha-tests.yml
#
# Root cause of the previous failure: saunafs-nfs-ganesha 5.6.0 references the
# global symbol `default_mutex_attr` (defined in ntirpc). Ubuntu's packaged
# libntirpc4.3t64 does NOT export this symbol in its dynamic table, causing
# dlopen to fail. Ganesha v9.2 ships ntirpc as a bundled submodule that DOES
# export the symbol — so we must build from source.

# GANESHA_VERSION: git tag of nfs-ganesha to build (e.g. V9.11)
ARG GANESHA_VERSION=V9.11

FROM ubuntu:24.04 AS ganesha-builder
ARG GANESHA_VERSION

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        acl \
        bison \
        build-essential \
        byacc \
        ca-certificates \
        cmake \
        dbus \
        flex \
        libacl1-dev \
        libblkid-dev \
        libboost-filesystem-dev \
        libboost-iostreams-dev \
        libboost-program-options-dev \
        libboost-system-dev \
        libcap-dev \
        libdbus-1-dev \
        libjemalloc-dev \
        libjudy-dev \
        libnfsidmap-dev \
        libnsl-dev \
        libsqlite3-dev \
        libtirpc-dev \
        liburcu-dev \
        pkg-config \
        wget \
    && rm -rf /var/lib/apt/lists/*

# Download ganesha ${GANESHA_VERSION} + its pinned ntirpc submodule commit via wget (avoids
# git SSL issues in restricted build environments). ntirpc commit pinned by
# ganesha V9.11 (same as V9.2): 366b5c3c1f8cb4090df942ce57e9913be96406a9
# -DCMAKE_INSTALL_LIBDIR=lib/x86_64-linux-gnu pins the FSAL destination to
# /usr/lib/x86_64-linux-gnu/ganesha/ — the same path the SaunaFS apt package
# uses — so no symlink juggling is needed in the runtime stage.
RUN set -eux; \
    wget -q -O /tmp/ganesha.tar.gz \
        "https://github.com/nfs-ganesha/nfs-ganesha/archive/refs/tags/${GANESHA_VERSION}.tar.gz"; \
    wget -q -O /tmp/ntirpc.tar.gz \
        https://github.com/nfs-ganesha/ntirpc/archive/366b5c3c1f8cb4090df942ce57e9913be96406a9.tar.gz; \
    mkdir -p /usr/src/nfs-ganesha /usr/src/nfs-ganesha/src/libntirpc; \
    tar -xzf /tmp/ganesha.tar.gz -C /usr/src/nfs-ganesha --strip-components=1; \
    tar -xzf /tmp/ntirpc.tar.gz -C /usr/src/nfs-ganesha/src/libntirpc --strip-components=1; \
    rm -f /tmp/ganesha.tar.gz /tmp/ntirpc.tar.gz; \
    cmake -B /usr/src/nfs-ganesha/build /usr/src/nfs-ganesha/src \
        -DCMAKE_C_FLAGS="-Wno-unused-function" \
        -DCMAKE_INSTALL_PREFIX=/usr \
        -DCMAKE_INSTALL_LIBDIR=lib/x86_64-linux-gnu \
        -DUSE_FSAL_CEPH=OFF \
        -DUSE_FSAL_GLUSTER=OFF \
        -DUSE_FSAL_GPFS=OFF \
        -DUSE_FSAL_KVSFS=OFF \
        -DUSE_FSAL_LIZARDFS=OFF \
        -DUSE_FSAL_LUSTRE=OFF \
        -DUSE_FSAL_SAUNAFS=OFF \
        -DUSE_FSAL_PROXY_V3=OFF \
        -DUSE_FSAL_PROXY_V4=OFF \
        -DUSE_FSAL_RGW=OFF \
        -DUSE_FSAL_XFS=OFF \
        -DUSE_GSS=OFF \
        -DUSE_MONITORING=OFF; \
    # Use DESTDIR so every file ganesha installs ends up under /tmp/ganesha-install/
    # giving us an exact manifest to COPY into the runtime stage.
    DESTDIR=/tmp/ganesha-install \
        cmake --build /usr/src/nfs-ganesha/build --target install -- -j$(($(nproc)*3/4+1)); \
    # Ensure the FSAL directory always exists (saunafs-nfs-ganesha will place
    # libfsalsaunafs.so there via apt in the runtime stage).
    mkdir -p /tmp/ganesha-install/usr/lib/x86_64-linux-gnu/ganesha; \
    rm -rf /usr/src/nfs-ganesha

# ── Stage 2: minimal runtime with Ganesha + SaunaFS FSAL ──────────────────────
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# Runtime deps: rpcbind (portmapper) + nfs-common (statd/idmapd)
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        dirmngr \
        gnupg \
        libacl1 \
        libblkid1 \
        libboost-filesystem1.83.0 \
        libboost-iostreams1.83.0 \
        libboost-program-options1.83.0 \
        libcap2 \
        libdbus-1-3 \
        libjemalloc2 \
        libjudydebian1 \
        libnsl2 \
        libsqlite3-0 \
        libtirpc3t64 \
        liburcu8t64 \
        nfs-common \
        rpcbind \
        wget \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# Copy everything ganesha installed (captured via DESTDIR in the builder).
# /tmp/ganesha-install/usr/ → /usr/ and /tmp/ganesha-install/etc/ → /etc/
# This includes: ganesha.nfsd, libganesha_nfsd.so.*, libntirpc.so.*, ganesha/ FSAL dir.
COPY --from=ganesha-builder /tmp/ganesha-install/usr/ /usr/
COPY --from=ganesha-builder /tmp/ganesha-install/etc/ /etc/

# Install SaunaFS lib-client + FSAL from the official SaunaFS apt repo.
# saunafs-nfs-ganesha is compiled against the Ganesha v9 ABI (including
# default_mutex_attr from bundled ntirpc), which V9.11 still ships via the
# same ntirpc commit pinned above.
RUN mkdir -p /root/.gnupg && chmod 700 /root/.gnupg \
    && update-ca-certificates \
    && gpg --no-default-keyring \
        --keyring /usr/share/keyrings/saunafs-archive-keyring.gpg \
        --keyserver hkps://keyserver.ubuntu.com \
        --receive-keys 0xA80B96E2C79457D4 \
    && echo "deb [arch=amd64 signed-by=/usr/share/keyrings/saunafs-archive-keyring.gpg] https://repo.saunafs.com/repository/saunafs-ubuntu-24.04/ noble main" \
        > /etc/apt/sources.list.d/saunafs.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        saunafs-lib-client \
        saunafs-nfs-ganesha \
    && apt-get clean && rm -rf /var/lib/apt/lists/* \
    # Ganesha's compiled-in MODULEDIR is /usr/lib/ganesha/ but the SaunaFS apt
    # package installs the FSAL to /usr/lib/x86_64-linux-gnu/ganesha/.
    # Symlink so both paths resolve to the same .so.
    && mkdir -p /usr/lib/ganesha \
    && ln -sf /usr/lib/x86_64-linux-gnu/ganesha/libfsalsaunafs.so \
              /usr/lib/ganesha/libfsalsaunafs.so \
    && ldconfig
