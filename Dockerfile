FROM ubuntu:16.04
MAINTAINER jdoliner@pachyderm.io

RUN \
  apt-get update -yq && \
  apt-get install -yq --no-install-recommends \
    apt-transport-https \
    software-properties-common \
    build-essential \
    ca-certificates \
    cmake \
    curl \
    fuse \
    git \
    libssl-dev \
    mercurial \
    pkg-config && \
  apt-get clean && \
  rm -rf /var/lib/apt
RUN \
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg | apt-key add - && \
  apt-key fingerprint 0EBFCD88 
RUN \
  add-apt-repository "deb https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable"
RUN \
  apt-get update && \
  apt-get install -yq --no-install-recommends \
    docker-ce
RUN \
  add-apt-repository ppa:gophers/archive && \
  apt update -yq && \
  apt-get install -yq --no-install-recommends \
  golang-1.8-go     
ENV PATH /go/bin:/usr/lib/go-1.8/bin:$PATH
ENV GOPATH /go
ENV GO15VENDOREXPERIMENT 1
RUN go get github.com/kisielk/errcheck github.com/golang/lint/golint
RUN mkdir -p /go/src/github.com/pachyderm/pachyderm
ADD . /go/src/github.com/pachyderm/pachyderm/
WORKDIR /go/src/github.com/pachyderm/pachyderm
