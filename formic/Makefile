SHA := $(shell git rev-parse --short HEAD)
VERSION := $(shell cat VERSION)
ITTERATION := $(shell date +%s)
LOCALPKGS :=  $(shell go list ./... | grep -v /vendor/)

deps:
	go get -u -f $(LOCALPKGS)

build:
	mkdir -p packaging/output
	mkdir -p packaging/root/usr/local/bin
	go build -i -v -o packaging/root/usr/local/bin/cfsdvp github.com/getcfs/megacfs/formic/cfsdvp

clean:
	rm -rf packaging/output
	rm -rf packaging/root/usr/local/bin

install: build
	cp -av packaging/root/usr/local/bin/* $(GOPATH)/bin

test:
	go test ./...

packages: clean deps build deb
