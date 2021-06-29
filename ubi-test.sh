#! /bin/bash

# build antrea/openvswitch-ubi
cd ./build/images/ovs

docker build --target ovs-rpms \
       --cache-from antrea/openvswitch-rpms:$OVS_VERSION \
       -t antrea/openvswitch-rpms:$OVS_VERSION \
       --build-arg OVS_VERSION=$OVS_VERSION \
       -f Dockerfile.ubi .

docker build \
       --cache-from antrea/openvswitch-rpms:$OVS_VERSION \
       --cache-from antrea/openvswitch-ubi:$OVS_VERSION \
       -t antrea/openvswitch-ubi:$OVS_VERSION \
       --build-arg OVS_VERSION=$OVS_VERSION \
       -f Dockerfile.ubi .

cd ./build/images/base

docker build \
        -t antrea/base-ubi:$OVS_VERSION \
        -f Dockerfile.ubi \
        --build-arg OVS_VERSION=$OVS_VERSION .

cd antrea

docker build -t antrea/antrea-ubi:$(DOCKER_IMG_VERSION) -f build/images/Dockerfile.build.ubi .