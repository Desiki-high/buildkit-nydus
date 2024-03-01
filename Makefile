prefix=/usr/local
bindir=$(prefix)/bin

binaries: FORCE
	hack/binaries

images: FORCE
# moby/buildkit:local and moby/buildkit:local-rootless are created on Docker
	hack/images local moby/buildkit
	TARGET=rootless hack/images local moby/buildkit

install: FORCE
	mkdir -p $(DESTDIR)$(bindir)
	install bin/* $(DESTDIR)$(bindir)

clean: FORCE
	rm -rf ./bin

test:
	./hack/test integration gateway dockerfile

lint:
	./hack/lint

validate-vendor:
	./hack/validate-vendor

validate-shfmt:
	./hack/validate-shfmt

shfmt:
	./hack/shfmt

validate-generated-files:
	./hack/validate-generated-files

validate-all: test lint validate-vendor validate-generated-files

vendor:
	./hack/update-vendor

generated-files:
	./hack/update-generated-files

.PHONY: vendor generated-files test binaries images install clean lint validate-all validate-vendor validate-generated-files
FORCE:

test:
	buildctl build --frontend=dockerfile.v0 \
	--output type=image,name=demo.goharbor.io/test/alpine:nydus-test,push=true,compression=nydus,nydus-fs-version=6,force-compression=true \
	--local dockerfile=.test/ \
	--local context=.test/
	nerdctl run --snapshotter nydus --name test demo.goharbor.io/test/alpine:nydus-test
	nerdctl stop test
	nerdctl rm test
	nerdctl rmi demo.goharbor.io/test/alpine:nydus-test

nydus-build:
	GOMAXPROCS=8 CGO_ENABLED=0 GOOS=linux go build -tags=nydus -o ./bin/buildctld ./cmd/buildctl
	GOMAXPROCS=8 CGO_ENABLED=0 GOOS=linux go build -tags=nydus -o ./bin/buildkitd ./cmd/buildkitd

nydus-install: nydus-build
	install bin/* /usr/local/bin
