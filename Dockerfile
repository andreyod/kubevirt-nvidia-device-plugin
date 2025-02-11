
ARG CUDA_IMAGE=cuda
ARG CUDA_VERSION=12.8.0
ARG BASE_DIST=ubi9

FROM nvcr.io/nvidia/${CUDA_IMAGE}:${CUDA_VERSION}-base-${BASE_DIST} as builder

RUN yum install -y wget make gcc

ARG GOLANG_VERSION=1.23.5
RUN wget -nv -O - https://storage.googleapis.com/golang/go${GOLANG_VERSION}.linux-amd64.tar.gz \
    | tar -C /usr/local -xz

ENV GOPATH /go
ENV PATH $GOPATH/bin:/usr/local/go/bin:$PATH

ENV GOOS=linux\
    GOARCH=amd64

WORKDIR /go/src/kubevirt-nvidia-device-plugin

COPY . . 

RUN make build

FROM nvcr.io/nvidia/${CUDA_IMAGE}:${CUDA_VERSION}-base-${BASE_DIST}

ARG VERSION

LABEL io.k8s.display-name="KubeVirt NVIDIA Device Plugin"
LABEL name="KubeVirt NVIDIA Device Plugin"
LABEL vendor="NVIDIA"

COPY --from=builder /go/src/kubevirt-nvidia-device-plugin/kubevirt-nvidia-device-plugin /usr/bin/
COPY --from=builder /go/src/kubevirt-nvidia-device-plugin/utils/pci.ids /usr/pci.ids

RUN yum update -y

CMD ["kubevirt-nvidia-device-plugin"]
