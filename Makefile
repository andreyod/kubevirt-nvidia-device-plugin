DOCKER_REPO ?= "localhost:5000/kubevirt-nvidia-device-plugin"
DOCKER_TAG ?= v1.0

PCI_IDS_URL ?= https://pci-ids.ucw.cz/v2.2/pci.ids

build:
	go build -o kubevirt-nvidia-device-plugin kubevirt-nvidia-device-plugin/cmd
build-image:
	docker build . -t $(DOCKER_REPO):$(DOCKER_TAG)
push-image: build-image
	 docker push $(DOCKER_REPO):$(DOCKER_TAG)
update-pcidb:
	wget $(PCI_IDS_URL) -O $(CURDIR)/utils/pci.ids
