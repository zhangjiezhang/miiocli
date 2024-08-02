FROM python:slim

ENV PYTHONUNBUFFERED 1
ENV CARGO_REGISTRIES_CRATES_IO_PROTOCOL=sparse

RUN ln -s /usr/bin/dpkg-split /usr/sbin/dpkg-split
RUN ln -s /usr/bin/dpkg-deb /usr/sbin/dpkg-deb
RUN ln -s /bin/rm /usr/sbin/rm
RUN ln -s /bin/tar /usr/sbin/tar

RUN apt-get clean \
    && apt-get update \
    && apt-get install -y build-essential libssl-dev libffi-dev python3-dev pkg-config \
       git curl ca-certificates gnupg netbase sq wget mercurial openssh-client subversion procps \
       autoconf automake bzip2 default-libmysqlclient-dev dpkg-dev file g++ gcc imagemagick libbz2-dev \
       libc6-dev libcurl4-openssl-dev libdb-dev libevent-dev libgdbm-dev libglib2.0-dev \
       libgmp-dev libjpeg-dev libkrb5-dev liblzma-dev libmagickcore-dev libmagickwand-dev libmaxminddb-dev \
       libncurses5-dev libncursesw5-dev libpng-dev libpq-dev libreadline-dev libsqlite3-dev \
       libtool libwebp-dev libxml2-dev libxslt-dev libyaml-dev make patch unzip xz-utils zlib1g-dev \
    && apt-get clean \
    && rm -rf /tmp/* /var/lib/apt/lists/* /var/tmp/*

LABEL org.opencontainers.image.source=https://github.com/rust-lang/docker-rust
ENV RUSTUP_HOME=/usr/local/rustup
ENV CARGO_HOME=/usr/local/cargo
ENV PATH=/usr/local/cargo/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ENV RUST_VERSION=1.80.0

RUN /bin/sh -c set -eux; \
       dpkgArch="$(dpkg --print-architecture)"; \
       case "${dpkgArch##*-}" in amd64) rustArch='x86_64-unknown-linux-gnu'; \
       rustupSha256='6aeece6993e902708983b209d04c0d1dbb14ebb405ddb87def578d41f920f56d' ;; armhf) rustArch='armv7-unknown-linux-gnueabihf'; \
       rustupSha256='3c4114923305f1cd3b96ce3454e9e549ad4aa7c07c03aec73d1a785e98388bed' ;; arm64) rustArch='aarch64-unknown-linux-gnu'; \
       rustupSha256='1cffbf51e63e634c746f741de50649bbbcbd9dbe1de363c9ecef64e278dba2b2' ;; i386) rustArch='i686-unknown-linux-gnu'; \
       rustupSha256='0a6bed6e9f21192a51f83977716466895706059afb880500ff1d0e751ada5237' ;; ppc64el) rustArch='powerpc64le-unknown-linux-gnu'; \
       rustupSha256='079430f58ad4da1d1f4f5f2f0bd321422373213246a93b3ddb53dad627f5aa38' ;; s390x) rustArch='s390x-unknown-linux-gnu'; \
       rustupSha256='e7f89da453c8ce5771c28279d1a01d5e83541d420695c74ec81a7ec5d287c51c' ;; *) echo >&2 "unsupported architecture: ${dpkgArch}"; \
       exit 1 ;; esac; \
       url="https://static.rust-lang.org/rustup/archive/1.27.1/${rustArch}/rustup-init"; \
       wget "$url"; \
       echo "${rustupSha256} *rustup-init" | sha256sum -c -; \
       chmod +x rustup-init; \
       ./rustup-init -y --no-modify-path --profile minimal --default-toolchain $RUST_VERSION --default-host ${rustArch}; \
       rm rustup-init; \
       chmod -R a+w $RUSTUP_HOME $CARGO_HOME; \
       rustup --version; \
       cargo --version; \
       rustc --version;

RUN pip install -U pip
RUN --mount=type=tmpfs,target=/root/.cargo pip install git+https://github.com/rytilahti/python-miio.git

RUN apt-get remove -y build-essential libssl-dev libffi-dev python3-dev pkg-config \
       git curl ca-certificates gnupg netbase sq wget mercurial openssh-client subversion procps \
       autoconf automake bzip2 default-libmysqlclient-dev dpkg-dev file g++ gcc imagemagick libbz2-dev \
       libc6-dev libcurl4-openssl-dev libdb-dev libevent-dev libgdbm-dev libglib2.0-dev \
       libgmp-dev libjpeg-dev libkrb5-dev liblzma-dev libmagickcore-dev libmagickwand-dev libmaxminddb-dev \
       libncurses5-dev libncursesw5-dev libpng-dev libpq-dev libreadline-dev libsqlite3-dev \
       libtool libwebp-dev libxml2-dev libxslt-dev libyaml-dev make patch unzip xz-utils zlib1g-dev \
    && apt-get autoremove -y
