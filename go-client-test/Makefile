all: upload

dstamp := $(shell date +%Y%m%d)
tstamp := $(shell date +%H%M%S)
timestamp ?= ${dstamp}-${tstamp}
version ?= latest
docker_repo ?= zongzw/zeebe-sshcmd

image_name := ${docker_repo}:${version}-${arch}-${timestamp}

upload: image
	docker push ${image_name}

image: binary
	docker buildx build \
		--no-cache \
		-t ${image_name} \
		-f Dockerfile-${arch} .

binary:
ifndef arch
	$(error 'arch' not defined, i.e, arch=arm64 arch=amd64)
endif
	CGO_ENABLED=0 GOOS=linux GOARCH=${arch} \
	go build -ldflags '-s -w --extldflags "-static -fpic"' -o sshcmd-${arch}; \
	# CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 \
	# go build -ldflags '-s -w --extldflags "-static -fpic"' -o sshcmd-${arch}

darwin:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags '-s -w --extldflags "-static -fpic"' -o sshcmd-darwin

