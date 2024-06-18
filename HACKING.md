# Hacking Guide

#dummy
#### Overview

This guide shows how to build the frr image using podman and a few ways to use it.

## Building the frr image

To build just use make

```
git clone https://github.com/openshift/frr.git
cd frr
sudo make
```

#### Using


#### To run the container on the host stack and use host pid

```
sudo podman run --privileged --pid=host --net=host -d --name="frr0" frr-daemonset:latest
```

You can check on things in various ways:

```
sudo podman exec -it frr0 /usr/lib/frr/frrinit.sh status
```

Obtain summary of bgp peers:

```
sudo podman exec frr0 vtysh 2501 -c "show ip bgp summary" 
```

Cleanup:

```
sudo podman stop frr0 ; sudo podman rm frr0
```

#### Running the container in its own networking stack 

You can also start multiple containers, each in their own network namespace, and have their FRR talk to each other:

```
sudo podman run --privileged -d --name="frr0" frr-daemonset:latest
sudo podman run --privileged -d --name="frr1" frr-daemonset:latest
```

The above will leverage podman networking, putting both containers in the same bridge.

Setting frr.conf values at runtime:

```
sudo podman run --privileged -d --network=podman99 --ip=10.99.0.11 --name="frr11" -e OCPPEER="10.99.0.12" -e OCPASN="64500" -e OCPROUTERID="10.99.0.11" frr-daemonset:latest

sudo podman run --privileged -d --network=podman99 --ip=10.99.0.12 --name="frr12" -e OCPPEER="10.99.0.11" -e OCPASN="64500" -e OCPROUETRID="10.99.0.12" frr-daemonset:latest 
```

Obtain status from frr: 

```
sudo podman exec -it frr11 /usr/lib/frr/frrinit.sh status
sudo podman exec -it frr12 /usr/lib/frr/frrinit.sh status
```

Cleaning up:

```
sudo podman stop frr11 ; sudo podman rm frr11
sudo podman stop frr12 ; sudo podman rm frr12
```

#### TODO

- Using docker RFC5549 can be used for the containers to automatically have each BGP container peer with the other.
- Using podman/CNI this doesn't work (reason TBD).  Currently peers need to start with static IPv4 addresses and know the remote peers static IP address


