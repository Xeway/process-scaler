# Process scaler

## Description

A program that starts a process and limits its resource usage during its execution.\
The process is limited so that resources are used at a 90% rate.
A 10% margin is left so that other processes can expand their resource usage if needed, without affecting their performance.\
The readjustment of the resource limits is done every second.

## Requirements

- Linux system
- cgroups v2

## Usage

```bash
sudo ./process_scaler <program> <args>
```

## Resources supported

Resources that are limited:
- CPU usage
- Memory usage
- (WIP)

## Usefulness

A major use case for this program is in the case you buy a whole server (you don't pay for what you use) but you only use a small part of it.\
What you can do is for example run a cryptocurrency miner to use the remaining resources. The problem is that the miner will use all the resources it can get, and the main process will probably be slowed down.\
This program solves this problem by limiting the resources of the miner to a certain percentage (keeps 10% of free resources), so that the main process can run without performance issues.
