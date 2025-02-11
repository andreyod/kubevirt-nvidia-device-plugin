DOCKER_REPO ?= "docker.io/kubevirt/kubevirt-nvidia-device-plugin"
DOCKER_TAG ?= v1.0

build:
	go build -o kubevirt-nvidia-device-plugin kubevirt-nvidia-device-plugin/cmd
build-image:
	docker build . -t $(DOCKER_REPO):$(DOCKER_TAG) 
push-image: build-image
	 docker push $(DOCKER_REPO):$(DOCKER_TAG)
